package controller

import (
	"fmt"
	"maps"
	"strconv"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// sandboxInjection derives the vars and secret bindings the selected sandbox
// provider requires on the control-plane worker. The names match what the
// upstream control plane reads from its environment.
func sandboxInjection(sb *openinspectv1alpha1.SandboxSpec) (vars map[string]string, secrets []openinspectv1alpha1.WorkerSecret) {
	vars = map[string]string{"SANDBOX_PROVIDER": sb.Provider}
	if sb.InactivityTimeoutMS > 0 {
		vars["SANDBOX_INACTIVITY_TIMEOUT_MS"] = strconv.FormatInt(sb.InactivityTimeoutMS, 10)
	}
	set := func(k, v string) {
		if v != "" {
			vars[k] = v
		}
	}
	sec := func(binding string, ref openinspectv1alpha1.SecretKeyRef) {
		secrets = append(secrets, openinspectv1alpha1.WorkerSecret{Binding: binding, SecretRef: ref})
	}

	switch sb.Provider {
	case "modal":
		m := sb.Modal
		set("MODAL_WORKSPACE", m.Workspace)
		set("MODAL_ENVIRONMENT", m.Environment)
		set("MODAL_ENVIRONMENT_WEB_SUFFIX", m.EnvironmentWebSuffix)
		sec("MODAL_TOKEN_ID", m.TokenIDSecretRef)
		sec("MODAL_TOKEN_SECRET", m.TokenSecretSecretRef)
		sec("MODAL_API_SECRET", m.APISecretSecretRef)
	case "daytona":
		d := sb.Daytona
		set("DAYTONA_API_URL", d.APIURL)
		set("DAYTONA_BASE_SNAPSHOT", d.BaseSnapshot)
		set("DAYTONA_TARGET", d.Target)
		sec("DAYTONA_API_KEY", d.APIKeySecretRef)
	case "vercel":
		v := sb.Vercel
		set("VERCEL_PROJECT_ID", v.ProjectID)
		set("VERCEL_RUNTIME", v.Runtime)
		set("VERCEL_TEAM_ID", v.TeamID)
		set("VERCEL_SANDBOX_API_BASE_URL", v.APIBaseURL)
		set("VERCEL_BASE_SNAPSHOT_ID", v.BaseSnapshotID)
		set("VERCEL_BASE_SNAPSHOT_NAME", v.BaseSnapshotName)
		if v.SnapshotExpirationMS > 0 {
			vars["VERCEL_SNAPSHOT_EXPIRATION_MS"] = strconv.FormatInt(v.SnapshotExpirationMS, 10)
		}
		sec("VERCEL_TOKEN", v.TokenSecretRef)
	case "opencomputer":
		o := sb.OpenComputer
		set("OPENCOMPUTER_API_URL", o.APIURL)
		set("OPENCOMPUTER_TEMPLATE", o.Template)
		sec("OPENCOMPUTER_API_KEY", o.APIKeySecretRef)
		if o.AnthropicAPIKeySecretRef != nil {
			sec("ANTHROPIC_API_KEY", *o.AnthropicAPIKeySecretRef)
		}
	}
	return vars, secrets
}

// applySandbox merges the provider injection into the target worker spec.
// Explicit values on the worker win; injection only fills gaps.
func applySandbox(oi *openinspectv1alpha1.OpenInspect, w openinspectv1alpha1.WorkerSpec) openinspectv1alpha1.WorkerSpec {
	sb := oi.Spec.Sandbox
	if sb == nil {
		return w
	}
	target := sb.ControlPlaneWorker
	if target == "" {
		target = "control-plane"
	}
	if w.Name != target {
		return w
	}

	vars, secrets := sandboxInjection(sb)

	// Cloudflare binding names share one namespace across kinds, so explicit
	// worker values suppress injected ones by NAME, var or secret alike.
	explicit := map[string]bool{}
	for k := range w.Vars {
		explicit[k] = true
	}
	for _, s := range w.Secrets {
		explicit[s.Binding] = true
	}

	merged := map[string]string{}
	for k, v := range vars {
		if !explicit[k] {
			merged[k] = v
		}
	}
	maps.Copy(merged, w.Vars)
	w.Vars = merged

	// w.Secrets came from a deepcopy of the CR; append must not alias it.
	out := make([]openinspectv1alpha1.WorkerSecret, len(w.Secrets), len(w.Secrets)+len(secrets))
	copy(out, w.Secrets)
	for _, s := range secrets {
		if !explicit[s.Binding] {
			out = append(out, s)
		}
	}
	w.Secrets = out
	return w
}

// validateSandbox checks provider/section consistency; called from
// validateSpec with the set of defined worker names.
func validateSandbox(sb *openinspectv1alpha1.SandboxSpec, workers map[string]bool) error {
	if sb == nil {
		return nil
	}
	target := sb.ControlPlaneWorker
	if target == "" {
		target = "control-plane"
	}
	if !workers[target] {
		return fmt.Errorf("sandbox.controlPlaneWorker %q is not defined in spec.workers", target)
	}
	// provider enum value -> json field name
	sections := map[string]struct {
		field   string
		present bool
	}{
		"modal":        {"modal", sb.Modal != nil},
		"daytona":      {"daytona", sb.Daytona != nil},
		"vercel":       {"vercel", sb.Vercel != nil},
		"opencomputer": {"openComputer", sb.OpenComputer != nil},
	}
	if !sections[sb.Provider].present {
		return fmt.Errorf("sandbox.provider is %q but spec.sandbox.%s is not set", sb.Provider, sections[sb.Provider].field)
	}
	for _, name := range []string{"modal", "daytona", "vercel", "opencomputer"} {
		if s := sections[name]; s.present && name != sb.Provider {
			return fmt.Errorf("sandbox.%s is set but provider is %q", s.field, sb.Provider)
		}
	}
	return nil
}
