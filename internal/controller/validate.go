package controller

import (
	"context"
	"fmt"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	"github.com/dirien/openinspect-operator/internal/platformbundle"
)

// claimConflict returns the namespace/name of an older OpenInspect that
// already claims the same account-level naming domain (ClaimKey), or "" if
// this CR is the rightful owner. Creation time (then namespace/name) decides
// ownership deterministically, so of two colliding CRs exactly one reconciles.
// The list is read live: an informer cache lagging a just-created claimant
// would let both CRs pass and capture each other's Cloudflare resources.
func (r *OpenInspectReconciler) claimConflict(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (string, error) {
	list := &openinspectv1alpha1.OpenInspectList{}
	if err := r.liveReader().List(ctx, list); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == oi.UID || other.ClaimKey() != oi.ClaimKey() {
			continue
		}
		if precedes(other, oi) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

// sharedModalApp returns the namespace/name of another live (not being
// deleted) OpenInspect whose Modal spec targets the same app — workspace plus
// environment NAME, the same key cleanupModalApp sweeps by (the web suffix
// only shapes URLs and is validated against name drift in validateSandbox) —
// or "" if this CR is the app's only user. Deleting siblings don't count, so
// of two concurrent deletions the app still gets stopped. Read live, not from
// the cache: this authorizes stopping an external app, and missing a
// just-created sibling would kill the pool out from under it.
func (r *OpenInspectReconciler) sharedModalApp(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (string, error) {
	list := &openinspectv1alpha1.OpenInspectList{}
	if err := r.liveReader().List(ctx, list); err != nil {
		return "", err
	}
	m := oi.Spec.Sandbox.Modal
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == oi.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		sb := other.Spec.Sandbox
		if sb == nil || sb.Provider != providerModal || sb.Modal == nil {
			continue
		}
		if sb.Modal.Workspace == m.Workspace && modalEnvironment(sb.Modal) == modalEnvironment(m) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

func precedes(a, b *openinspectv1alpha1.OpenInspect) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}

// validateSpec catches cross-field references the CRD schema cannot express:
// bindings pointing at undefined resources would otherwise upload empty IDs
// or stay pending forever.
func validateSpec(oi *openinspectv1alpha1.OpenInspect) error {
	kv := map[string]bool{}
	for _, n := range oi.Spec.KVNamespaces {
		kv[n] = true
	}
	r2 := map[string]bool{}
	for _, n := range oi.Spec.R2Buckets {
		r2[n] = true
	}
	workers := map[string]bool{}
	for _, w := range oi.Spec.Workers {
		if workers[w.Name] {
			return fmt.Errorf("duplicate worker name %q", w.Name)
		}
		workers[w.Name] = true
	}
	if err := validateSandbox(oi.Spec.Sandbox, workers); err != nil {
		return err
	}
	if err := validateModalAppInputs(oi); err != nil {
		return err
	}
	for _, w := range oi.Spec.Workers {
		if len(w.Bindings.D1) > 0 && oi.Spec.D1 == nil {
			return fmt.Errorf("worker %q has a d1 binding but spec.d1 is not set", w.Name)
		}
		for _, b := range w.Bindings.KV {
			if !kv[b.Namespace] {
				return fmt.Errorf("worker %q kv binding %q references undefined namespace %q", w.Name, b.Binding, b.Namespace)
			}
		}
		for _, b := range w.Bindings.R2 {
			if !r2[b.Bucket] {
				return fmt.Errorf("worker %q r2 binding %q references undefined bucket %q", w.Name, b.Binding, b.Bucket)
			}
		}
		for _, b := range w.Bindings.Services {
			if !workers[b.Worker] {
				return fmt.Errorf("worker %q service binding %q references undefined worker %q", w.Name, b.Binding, b.Worker)
			}
		}
		switch {
		case w.Script == nil:
			// No override means the release's embedded bundle; only known
			// platform workers have one.
			if !platformbundle.Has(w.Name) {
				names, _ := platformbundle.Names()
				return fmt.Errorf("worker %q has no built-in bundle (available: %v); set script.configMapRef or script.url", w.Name, names)
			}
		case (w.Script.ConfigMapRef == nil) == (w.Script.URL == ""):
			return fmt.Errorf("worker %q script needs exactly one of configMapRef or url", w.Name)
		}
	}
	return validateAppSecrets(oi, workers)
}

// appSecretsReservedKeys mirrors the upstream store's RESERVED_KEYS
// (control-plane secrets-validation.ts): a PUT containing any of them is
// rejected wholesale, so catch it here as a terminal spec error instead of
// an endless reconcile backoff.
var appSecretsReservedKeys = map[string]bool{
	"PYTHONUNBUFFERED": true, "SANDBOX_ID": true, "CONTROL_PLANE_URL": true,
	"SANDBOX_AUTH_TOKEN": true, "REPO_OWNER": true, "REPO_NAME": true,
	"GITHUB_APP_TOKEN": true, "SESSION_CONFIG": true, "RESTORED_FROM_SNAPSHOT": true,
	"OPENCODE_CONFIG_CONTENT": true, "PATH": true, "HOME": true, "USER": true,
	"SHELL": true, "TERM": true, "PWD": true, "LANG": true,
}

func validateAppSecrets(oi *openinspectv1alpha1.OpenInspect, workers map[string]bool) error {
	if len(oi.Spec.AppSecrets) == 0 {
		return nil
	}
	for _, s := range oi.Spec.AppSecrets {
		if appSecretsReservedKeys[s.Key] {
			return fmt.Errorf("appSecrets key %q is reserved by the platform", s.Key)
		}
	}
	name := controlPlaneWorkerName(oi)
	if !workers[name] {
		return fmt.Errorf("appSecrets requires control-plane worker %q in spec.workers", name)
	}
	for _, w := range oi.Spec.Workers {
		if w.Name != name {
			continue
		}
		for _, s := range w.Secrets {
			if s.Binding == internalCallbackSecretBinding {
				return nil
			}
		}
	}
	return fmt.Errorf("appSecrets requires an %s secret binding on worker %q", internalCallbackSecretBinding, name)
}
