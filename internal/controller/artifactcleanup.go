package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
	daytonaprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/daytona"
	modalprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/modal"
)

// retiredArtifactGracePeriod is how long a superseded managed artifact is
// retained for rollback before garbage collection (EPIC1 D6).
const retiredArtifactGracePeriod = 24 * time.Hour

// runtimeOrphaned reports whether the managed artifacts must be retained
// (spec.sandbox.runtime.deletionPolicy: Orphan).
func runtimeOrphaned(oi *openinspectv1alpha1.OpenInspect) bool {
	sb := oi.Spec.Sandbox
	return sb != nil && sb.Runtime != nil && sb.Runtime.DeletionPolicy == "Orphan"
}

// deleteRuntimeArtifact removes one managed Daytona artifact, through the
// test hook when set.
func (r *OpenInspectReconciler) deleteRuntimeArtifact(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, artifact provider.Artifact) error {
	if r.RuntimeDelete != nil {
		return r.RuntimeDelete(ctx, oi, artifact)
	}
	d := oi.Spec.Sandbox.Daytona
	apiKey, err := r.secretValue(ctx, oi.Namespace, d.APIKeySecretRef)
	if err != nil {
		return fmt.Errorf("daytona api key: %w", err)
	}
	p, err := daytonaprovider.New(daytonaprovider.Config{APIKey: apiKey, APIURL: d.APIURL, Target: d.Target})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return p.DeleteBaseArtifact(ctx, artifact)
}

// gcRetiredArtifacts deletes superseded managed artifacts whose rollback
// grace period expired. GC failures warn and retry on the next resync
// instead of blocking the reconcile: a temporarily unreachable provider must
// not take Ready down over an already-replaced artifact.
func (r *OpenInspectReconciler) gcRetiredArtifacts(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) {
	st := oi.Status.SandboxRuntime
	if st == nil || len(st.PreviousArtifacts) == 0 || runtimeOrphaned(oi) {
		return
	}
	var kept []openinspectv1alpha1.RetiredArtifact
	for _, retired := range st.PreviousArtifacts {
		if time.Since(retired.RetiredAt.Time) < retiredArtifactGracePeriod {
			kept = append(kept, retired)
			continue
		}
		if err := r.deleteRuntimeArtifact(ctx, oi, provider.Artifact{Name: retired.Name, ID: retired.ID}); err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "RuntimeArtifactGCFailed",
				"could not delete retired artifact %s: %v (will retry)", retired.Name, err)
			kept = append(kept, retired)
			continue
		}
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeArtifactDeleted",
			"garbage-collected retired artifact %s (grace period %s elapsed)", retired.Name, retiredArtifactGracePeriod)
	}
	st.PreviousArtifacts = kept
}

// cleanupManagedArtifact deletes the managed base artifact (active and
// retired) during finalization, or records the retained references when
// runtime.deletionPolicy is Orphan.
func (r *OpenInspectReconciler) cleanupManagedArtifact(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) error {
	sb := oi.Spec.Sandbox
	st := oi.Status.SandboxRuntime
	if sb == nil || sb.Provider != providerDaytona || st == nil {
		return nil
	}
	if st.Phase == openinspectv1alpha1.RuntimePhaseExternal {
		// Externally owned, used as-is — never deleted (EPIC1 section 4).
		return nil
	}
	refs := make([]string, 0, len(st.PreviousArtifacts)+1)
	if st.ArtifactName != "" {
		refs = append(refs, st.ArtifactName)
	}
	for _, retired := range st.PreviousArtifacts {
		refs = append(refs, retired.Name)
	}
	if len(refs) == 0 {
		return nil
	}
	if runtimeOrphaned(oi) {
		// The complete references live in status until the object goes; the
		// event is the durable record for after that.
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeArtifactsOrphaned",
			"retaining managed daytona artifacts per runtime.deletionPolicy Orphan: %s", strings.Join(refs, ", "))
		return nil
	}
	if st.ArtifactName != "" {
		if err := r.deleteRuntimeArtifact(ctx, oi, provider.Artifact{Name: st.ArtifactName, ID: st.ArtifactID}); err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "RuntimeArtifactDeletionFailed",
				"%v (set deletionPolicy: Orphan to skip)", err)
			return err
		}
	}
	for _, retired := range st.PreviousArtifacts {
		if err := r.deleteRuntimeArtifact(ctx, oi, provider.Artifact{Name: retired.Name, ID: retired.ID}); err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "RuntimeArtifactDeletionFailed",
				"%v (set deletionPolicy: Orphan to skip)", err)
			return err
		}
	}
	r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeArtifactsDeleted",
		"deleted %d managed daytona artifacts", len(refs))
	return nil
}

// deleteProviderImage removes one recorded prebuild image, through the test
// hook when set.
func (r *OpenInspectReconciler) deleteProviderImage(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, ref cfclient.ImageRef) error {
	if r.ProviderImageDelete != nil {
		return r.ProviderImageDelete(ctx, oi, ref)
	}
	sb := oi.Spec.Sandbox
	switch ref.Provider {
	case providerModal:
		if sb == nil || sb.Modal == nil {
			return fmt.Errorf("image %s belongs to provider modal but the spec no longer configures it", ref.ImageID)
		}
		tokenID, err := r.secretValue(ctx, oi.Namespace, sb.Modal.TokenIDSecretRef)
		if err != nil {
			return err
		}
		tokenSecret, err := r.secretValue(ctx, oi.Namespace, sb.Modal.TokenSecretSecretRef)
		if err != nil {
			return err
		}
		p, err := modalprovider.New(modalprovider.Config{
			TokenID: tokenID, TokenSecret: tokenSecret,
			Workspace: sb.Modal.Workspace, Environment: modalEnvironment(sb.Modal),
		})
		if err != nil {
			return err
		}
		return p.DeleteImage(ctx, ref.ImageID)
	case providerDaytona:
		if sb == nil || sb.Daytona == nil {
			return fmt.Errorf("image %s belongs to provider daytona but the spec no longer configures it", ref.ImageID)
		}
		return r.deleteRuntimeArtifact(ctx, oi, provider.Artifact{Name: ref.ImageID})
	default:
		// Providers the operator does not manage (D3): leave their images
		// alone rather than pretending to know how to delete them.
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "ProviderImageSkipped",
			"not deleting image %s of unmanaged provider %q", ref.ImageID, ref.Provider)
		return nil
	}
}

// cleanupProviderImages deletes the repo/environment prebuild images the
// platform recorded in the CR-owned D1 image_builds table (EPIC1 section 8
// step 2). Runs before D1 deletion — D1 is the evidence.
func (r *OpenInspectReconciler) cleanupProviderImages(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) error {
	if oi.Spec.Sandbox == nil || oi.Status.D1DatabaseID == "" {
		return nil
	}
	// The whole Modal app (and its images) may still be in use by a sibling
	// CR sharing the workspace+environment; that guard covers images too.
	if m := oi.Spec.Sandbox.Modal; m != nil {
		user, err := r.sharedModalApp(ctx, oi)
		if err != nil {
			return err
		}
		if user != "" {
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ProviderImageCleanupSkipped",
				"modal app still in use by OpenInspect %s; leaving its images alone", user)
			return nil
		}
	}
	refs, found, err := cf.D1ImageBuildRefs(ctx, oi.Status.D1DatabaseID)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "ProviderImageEvidenceReadFailed",
			"%v (set deletionPolicy: Orphan to skip)", err)
		return err
	}
	if !found {
		// Older schema or vanished database: no image evidence can exist.
		return nil
	}
	deleted := 0
	for _, ref := range refs {
		if err := r.deleteProviderImage(ctx, oi, ref); err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "ProviderImageDeletionFailed",
				"image %s (%s): %v (set deletionPolicy: Orphan to skip)", ref.ImageID, ref.Provider, err)
			return err
		}
		deleted++
	}
	if deleted > 0 {
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ProviderImagesDeleted",
			"deleted %d recorded prebuild images", deleted)
	}
	return nil
}
