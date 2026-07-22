package controller

import (
	"fmt"
	"maps"
	"strconv"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// sandboxInjection derives the vars and secret bindings the selected sandbox
// provider requires on the control-plane worker. The names match what the
// upstream control plane reads from its environment. Managed-artifact values
// (the Daytona base snapshot, the Modal environment web suffix) come from
// status: the operator built or discovered them, the user never supplies them.
func sandboxInjection(oi *openinspectv1alpha1.OpenInspect) (vars map[string]string, secrets []openinspectv1alpha1.WorkerSecret) {
	sb := oi.Spec.Sandbox
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
	case providerModal:
		m := sb.Modal
		set("MODAL_WORKSPACE", m.Workspace)
		set("MODAL_ENVIRONMENT", m.Environment)
		// Discovered from the deployed app's endpoints, not configured: the
		// operator deploys the app and records the suffix in status.
		if app := oi.Status.ModalApp; app != nil {
			set("MODAL_ENVIRONMENT_WEB_SUFFIX", app.WebSuffix)
		}
		sec("MODAL_TOKEN_ID", m.TokenIDSecretRef)
		sec("MODAL_TOKEN_SECRET", m.TokenSecretSecretRef)
		sec("MODAL_API_SECRET", m.APISecretSecretRef)
	case "daytona":
		d := sb.Daytona
		set("DAYTONA_API_URL", d.APIURL)
		set("DAYTONA_BASE_SNAPSHOT", daytonaSnapshotName(oi))
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

// daytonaSnapshotName returns the base snapshot the control plane must boot
// sandboxes from: the explicit external reference when configured, otherwise
// the managed artifact recorded in status (empty until its first build
// finishes).
func daytonaSnapshotName(oi *openinspectv1alpha1.OpenInspect) string {
	if d := oi.Spec.Sandbox.Daytona; d != nil && d.BaseSnapshot != "" {
		return d.BaseSnapshot
	}
	if rt := oi.Status.SandboxRuntime; rt != nil {
		return rt.ArtifactName
	}
	return ""
}

// applySandbox merges the provider injection into the target worker spec.
// Explicit values on the worker win; injection only fills gaps.
func applySandbox(oi *openinspectv1alpha1.OpenInspect, w openinspectv1alpha1.WorkerSpec) openinspectv1alpha1.WorkerSpec {
	sb := oi.Spec.Sandbox
	if sb == nil {
		return w
	}
	if w.Name != controlPlaneWorkerName(oi) {
		return w
	}

	vars, secrets := sandboxInjection(oi)

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
		providerModal:  {providerModal, sb.Modal != nil},
		"daytona":      {"daytona", sb.Daytona != nil},
		"vercel":       {"vercel", sb.Vercel != nil},
		"opencomputer": {"openComputer", sb.OpenComputer != nil},
	}
	if !sections[sb.Provider].present {
		return fmt.Errorf("sandbox.provider is %q but spec.sandbox.%s is not set", sb.Provider, sections[sb.Provider].field)
	}
	for _, name := range []string{providerModal, "daytona", "vercel", "opencomputer"} {
		if s := sections[name]; s.present && name != sb.Provider {
			return fmt.Errorf("sandbox.%s is set but provider is %q", s.field, sb.Provider)
		}
	}
	if rt := sb.Runtime; rt != nil {
		// Managed base artifacts exist only for the providers the pinned
		// upstream revision actually drives (EPIC1 D3).
		if sb.Provider != providerModal && sb.Provider != providerDaytona {
			return fmt.Errorf("sandbox.runtime is only supported for the managed providers (modal, daytona), not %q", sb.Provider)
		}
		// The Modal base image is defined inside the unmodified upstream app
		// (EPIC1 D2); there is nothing an image override could apply to.
		if rt.Image != "" && sb.Provider == providerModal {
			return fmt.Errorf("sandbox.runtime.image cannot be set for provider modal: the base image is defined by the upstream Modal app")
		}
	}
	return nil
}

// validateModalAppInputs checks that everything the operator-deployed
// upstream Modal app resolves at deploy time (its three named secrets) is
// present in the spec — a deploy without them fails remotely, so catch it
// as a terminal InvalidSpec instead of an endless Job retry loop.
func validateModalAppInputs(oi *openinspectv1alpha1.OpenInspect) error {
	sb := oi.Spec.Sandbox
	if sb == nil || sb.Provider != providerModal {
		return nil
	}
	hasAnthropic := false
	for _, s := range oi.Spec.AppSecrets {
		if s.Key == "ANTHROPIC_API_KEY" {
			hasAnthropic = true
		}
	}
	if !hasAnthropic {
		return fmt.Errorf("provider modal requires an ANTHROPIC_API_KEY entry in spec.appSecrets: the upstream Modal app resolves it as its llm-api-keys secret")
	}
	name := controlPlaneWorkerName(oi)
	bindings := map[string]bool{}
	for _, w := range oi.Spec.Workers {
		if w.Name != name {
			continue
		}
		for _, s := range w.Secrets {
			bindings[s.Binding] = true
		}
	}
	for _, required := range modalGitHubAppBindings {
		if !bindings[required] {
			return fmt.Errorf("provider modal requires a %s secret binding on worker %q: the upstream Modal app resolves it as its github-app secret", required, name)
		}
	}
	if !bindings[internalCallbackSecretBinding] {
		return fmt.Errorf("provider modal requires an %s secret binding on worker %q: the upstream Modal app resolves it as its internal-api secret", internalCallbackSecretBinding, name)
	}
	return nil
}
