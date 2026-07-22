package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// referencesConfigMap reports whether the CR reads the named ConfigMap.
func referencesConfigMap(oi *openinspectv1alpha1.OpenInspect, name string) bool {
	if oi.Spec.D1 != nil && oi.Spec.D1.MigrationsConfigMapRef == name {
		return true
	}
	for _, w := range oi.Spec.Workers {
		if w.Script != nil && w.Script.ConfigMapRef != nil && w.Script.ConfigMapRef.Name == name {
			return true
		}
	}
	return false
}

// referencesSecret reports whether the CR reads the named Secret.
func referencesSecret(oi *openinspectv1alpha1.OpenInspect, name string) bool {
	if oi.Spec.Cloudflare.APITokenSecretRef.Name == name {
		return true
	}
	for _, w := range oi.Spec.Workers {
		for _, s := range w.Secrets {
			if s.SecretRef.Name == name {
				return true
			}
		}
	}
	if web := oi.Spec.Web; web != nil {
		for _, s := range web.SecretEnv {
			if s.SecretRef.Name == name {
				return true
			}
		}
	}
	for _, s := range oi.Spec.AppSecrets {
		if s.SecretRef.Name == name {
			return true
		}
	}
	if sb := oi.Spec.Sandbox; sb != nil {
		refs := []openinspectv1alpha1.SecretKeyRef{}
		if sb.Modal != nil {
			refs = append(refs, sb.Modal.TokenIDSecretRef, sb.Modal.TokenSecretSecretRef, sb.Modal.APISecretSecretRef)
		}
		if sb.Daytona != nil {
			refs = append(refs, sb.Daytona.APIKeySecretRef)
		}
		if sb.Vercel != nil {
			refs = append(refs, sb.Vercel.TokenSecretRef)
		}
		if sb.OpenComputer != nil {
			refs = append(refs, sb.OpenComputer.APIKeySecretRef)
			if sb.OpenComputer.AnthropicAPIKeySecretRef != nil {
				refs = append(refs, *sb.OpenComputer.AnthropicAPIKeySecretRef)
			}
		}
		for _, ref := range refs {
			if ref.Name == name {
				return true
			}
		}
	}
	return false
}

// mapReferencing enqueues every OpenInspect in the object's namespace that
// references it, so ConfigMap/Secret edits roll out without waiting for the
// periodic resync. CR counts per namespace are small; list-and-filter beats
// maintaining field indexes.
func (r *OpenInspectReconciler) mapReferencing(matches func(*openinspectv1alpha1.OpenInspect, string) bool) func(context.Context, client.Object) []ctrl.Request {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		list := &openinspectv1alpha1.OpenInspectList{}
		if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			logf.FromContext(ctx).Error(err, "listing OpenInspects for watch mapping")
			return nil
		}
		var reqs []ctrl.Request
		for i := range list.Items {
			if matches(&list.Items[i], obj.GetName()) {
				reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
					Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
				}})
			}
		}
		return reqs
	}
}
