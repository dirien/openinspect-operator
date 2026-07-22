package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
	daytonaprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/daytona"
)

const (
	// runtimeBuildRetryInterval is how long a failed build Job blocks before
	// the operator deletes and recreates it. The Job's own backoffLimit
	// handles transient retries; this guards against systemic failures
	// hammering the provider.
	runtimeBuildRetryInterval = 10 * time.Minute

	// runtimeInputsHashAnnotation detects build Jobs whose non-digest inputs
	// (endpoints, secret refs, builder image) went stale.
	runtimeInputsHashAnnotation = "openinspect.dirien.dev/inputs-hash"

	runtimeBuilderContainer = "runtime-builder"
)

// runtimeDigests returns the full digest recorded in status and the short
// form embedded in artifact names. With a base-image override the artifact
// content is the override image, so the digest derives from that reference
// instead of the bundled manifest.
func runtimeDigests(sb *openinspectv1alpha1.SandboxSpec) (full, short string, err error) {
	if rt := sb.Runtime; rt != nil && rt.Image != "" {
		sum := sha256.Sum256([]byte("base-image-override\x00" + rt.Image))
		full = "sha256:" + hex.EncodeToString(sum[:])
		return full, full[len("sha256:") : len("sha256:")+12], nil
	}
	m, err := sandboxruntime.Load()
	if err != nil {
		return "", "", err
	}
	if full, err = m.Digest(); err != nil {
		return "", "", err
	}
	if short, err = m.ShortDigest(); err != nil {
		return "", "", err
	}
	return full, short, nil
}

// runtimeArtifactName is the deterministic per-CR artifact name: the UID
// prefix isolates installations sharing one provider account, the digest
// makes a changed bundle a new artifact instead of a mutation.
func runtimeArtifactName(oi *openinspectv1alpha1.OpenInspect, shortDigest string) string {
	uid := strings.ReplaceAll(string(oi.UID), "-", "")
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return "oi-" + uid + "-" + shortDigest
}

// runtimeBuildJobName stays within the 63-character object name limit.
func runtimeBuildJobName(oi *openinspectv1alpha1.OpenInspect, shortDigest string) string {
	base := oi.Name
	if limit := 63 - len("-rtb-") - len(shortDigest); len(base) > limit {
		base = base[:limit]
	}
	return base + "-rtb-" + shortDigest
}

// ensureSandboxRuntime reconciles the managed provider base artifact:
// status.sandboxRuntime, the RuntimeReady condition, and the runtime-build
// Job. It never blocks on the remote build (EPIC1 section 6) — the Job does
// the building, the controller only inspects and reflects. pending reports
// whether the artifact is not usable yet so the caller keeps Ready=False
// with a quick requeue.
func (r *OpenInspectReconciler) ensureSandboxRuntime(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (pending bool, err error) {
	sb := oi.Spec.Sandbox
	if sb == nil || sb.Provider != providerDaytona {
		// No provider-owned artifact to manage: Modal's base image belongs
		// to the deployed upstream app (ensureModalApp owns the RuntimeReady
		// condition there); other providers have no managed artifact (D3).
		oi.Status.SandboxRuntime = nil
		if sb == nil || sb.Provider != providerModal {
			meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionRuntime)
		}
		return false, nil
	}
	d := sb.Daytona

	// External reference: used as-is, never built, inspected, or deleted.
	if d.BaseSnapshot != "" {
		oi.Status.SandboxRuntime = &openinspectv1alpha1.SandboxRuntimeStatus{
			Provider:           providerDaytona,
			Phase:              openinspectv1alpha1.RuntimePhaseExternal,
			ArtifactName:       d.BaseSnapshot,
			ObservedGeneration: oi.Generation,
		}
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionTrue,
			"ExternalArtifact", fmt.Sprintf("using externally owned snapshot %q as-is", d.BaseSnapshot))
		return false, nil
	}

	full, short, err := runtimeDigests(sb)
	if err != nil {
		return false, fmt.Errorf("runtime digest: %w", err)
	}
	target := runtimeTarget{fullDigest: full, shortDigest: short, name: runtimeArtifactName(oi, short)}

	st := oi.Status.SandboxRuntime
	if st == nil {
		st = &openinspectv1alpha1.SandboxRuntimeStatus{}
		oi.Status.SandboxRuntime = st
	}
	st.Provider = providerDaytona
	st.ObservedGeneration = oi.Generation

	// ArtifactName/Digest always describe the ACTIVE artifact — the one the
	// control plane is wired to. During an upgrade build the old reference
	// keeps serving; it flips (and the old artifact is retired for
	// grace-period GC, D6) only once the new artifact is confirmed ready.
	if sb.Runtime != nil && sb.Runtime.UpdatePolicy == "Never" &&
		st.Phase == openinspectv1alpha1.RuntimePhaseReady &&
		st.ArtifactName != "" && st.ArtifactName != target.name {
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionTrue,
			"UpdatePolicyNever", fmt.Sprintf("holding artifact %s; digest %s not rolled out (updatePolicy: Never)", st.ArtifactName, target.shortDigest))
		return false, nil
	}

	// Superseded artifacts past their rollback grace period are collected
	// here, on the ordinary reconcile cadence (D6).
	r.gcRetiredArtifacts(ctx, oi)

	job := &batchv1.Job{}
	jobName := runtimeBuildJobName(oi, target.shortDigest)
	jobErr := r.Get(ctx, types.NamespacedName{Namespace: oi.Namespace, Name: jobName}, job)
	if jobErr != nil && client.IgnoreNotFound(jobErr) != nil {
		return false, jobErr
	}
	if jobErr == nil {
		return r.runtimeFromJob(ctx, oi, st, job, jobName, target)
	}
	return r.runtimeFromRemote(ctx, oi, st, jobName, target)
}

// runtimeTarget is the artifact the CR converges toward this reconcile.
type runtimeTarget struct {
	fullDigest  string
	shortDigest string
	name        string
}

// activateArtifact flips the active reference to the confirmed-ready target,
// retiring a superseded artifact for grace-period GC (D6).
func (r *OpenInspectReconciler) activateArtifact(oi *openinspectv1alpha1.OpenInspect, st *openinspectv1alpha1.SandboxRuntimeStatus, target runtimeTarget, id string) {
	if st.ArtifactName != "" && st.ArtifactName != target.name {
		r.retireArtifact(oi, st)
	}
	st.ArtifactName = target.name
	st.ArtifactID = id
	st.Digest = target.fullDigest
	st.SourceRevision = sandboxruntime.UpstreamCommit()
	st.Phase = openinspectv1alpha1.RuntimePhaseReady
	r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionTrue,
		"Reconciled", fmt.Sprintf("artifact %s ready (digest %s)", target.name, target.shortDigest))
}

// runtimeFromJob reflects an existing build Job into status: success is
// verified against the remote artifact, failure is retried after a
// cool-down, and stale inputs replace the Job.
func (r *OpenInspectReconciler) runtimeFromJob(
	ctx context.Context,
	oi *openinspectv1alpha1.OpenInspect,
	st *openinspectv1alpha1.SandboxRuntimeStatus,
	job *batchv1.Job,
	jobName string,
	target runtimeTarget,
) (pending bool, err error) {
	switch {
	case jobSucceeded(job):
		state, err := r.inspectRuntimeArtifact(ctx, oi, provider.Artifact{Name: target.name})
		if err != nil {
			return true, err
		}
		if state.Phase == provider.ArtifactReady {
			if t := jobCompletionTime(job); t != nil {
				st.LastBuildTime = t
			} else {
				now := metav1.Now()
				st.LastBuildTime = &now
			}
			r.activateArtifact(oi, st, target, state.ID)
			return false, nil
		}
		// The Job claims success but the artifact is not usable: surface
		// it instead of trusting the exit code.
		st.Phase = openinspectv1alpha1.RuntimePhaseFailed
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse,
			"RuntimeArtifactMissing", fmt.Sprintf("build job succeeded but artifact %s is %s: %s", target.name, state.Phase, state.Message))
		return true, nil
	case jobFailed(job):
		st.Phase = openinspectv1alpha1.RuntimePhaseFailed
		msg := jobFailureMessage(job)
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse, "RuntimeBuildFailed",
			fmt.Sprintf("build job %s failed: %s (retrying after %s)", jobName, msg, runtimeBuildRetryInterval))
		// Recreate after the cool-down so systemic failures don't hammer
		// the provider.
		if t := jobFailureTime(job); t != nil && time.Since(t.Time) > runtimeBuildRetryInterval {
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && client.IgnoreNotFound(err) != nil {
				return true, err
			}
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeBuildRetried",
				"recreating failed build job %s", jobName)
		}
		return true, nil
	case job.Annotations[runtimeInputsHashAnnotation] != r.runtimeJobInputsHash(oi):
		// Non-digest inputs changed (endpoint, secret ref, builder
		// image): replace the Job.
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && client.IgnoreNotFound(err) != nil {
			return true, err
		}
		st.Phase = openinspectv1alpha1.RuntimePhasePending
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse,
			"RuntimeBuildPending", "build job inputs changed; recreating")
		return true, nil
	default:
		st.Phase = openinspectv1alpha1.RuntimePhaseBuilding
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse,
			"RuntimeBuilding", fmt.Sprintf("build job %s in progress", jobName))
		return true, nil
	}
}

// runtimeFromRemote reconciles without a build Job: the artifact may already
// exist (operator restart, Job TTL expiry, or a pre-existing build), so it
// is inspected before any build is started.
func (r *OpenInspectReconciler) runtimeFromRemote(
	ctx context.Context,
	oi *openinspectv1alpha1.OpenInspect,
	st *openinspectv1alpha1.SandboxRuntimeStatus,
	jobName string,
	target runtimeTarget,
) (pending bool, err error) {
	state, err := r.inspectRuntimeArtifact(ctx, oi, provider.Artifact{Name: target.name})
	if err != nil {
		return true, err
	}
	switch state.Phase {
	case provider.ArtifactReady:
		r.activateArtifact(oi, st, target, state.ID)
		return false, nil
	case provider.ArtifactBuilding:
		st.Phase = openinspectv1alpha1.RuntimePhaseBuilding
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse,
			"RuntimeBuilding", fmt.Sprintf("artifact %s building on the provider", target.name))
		return true, nil
	case provider.ArtifactAbsent, provider.ArtifactFailed:
		if err := r.createRuntimeBuildJob(ctx, oi, jobName, target.name); err != nil {
			return true, err
		}
		st.Phase = openinspectv1alpha1.RuntimePhasePending
		r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionFalse,
			"RuntimeBuildPending", fmt.Sprintf("build job %s created for artifact %s", jobName, target.name))
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeBuildStarted",
			"building %s artifact %s (digest %s)", providerDaytona, target.name, target.shortDigest)
		return true, nil
	}
	return true, fmt.Errorf("unhandled artifact phase %q", state.Phase)
}

// retireArtifact moves the current managed artifact into the grace-period
// GC list, deduplicated by name and bounded to the API's MaxItems.
func (r *OpenInspectReconciler) retireArtifact(oi *openinspectv1alpha1.OpenInspect, st *openinspectv1alpha1.SandboxRuntimeStatus) {
	for _, prev := range st.PreviousArtifacts {
		if prev.Name == st.ArtifactName {
			return
		}
	}
	st.PreviousArtifacts = append(st.PreviousArtifacts, openinspectv1alpha1.RetiredArtifact{
		Name: st.ArtifactName, ID: st.ArtifactID, RetiredAt: metav1.Now(),
	})
	if len(st.PreviousArtifacts) > 10 {
		st.PreviousArtifacts = st.PreviousArtifacts[len(st.PreviousArtifacts)-10:]
	}
	r.Recorder.Eventf(oi, corev1.EventTypeNormal, "RuntimeArtifactRetired",
		"artifact %s superseded; retained for rollback grace period", st.ArtifactName)
}

// inspectRuntimeArtifact resolves the provider's view of the artifact,
// through the test hook when set.
func (r *OpenInspectReconciler) inspectRuntimeArtifact(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, artifact provider.Artifact) (provider.ArtifactState, error) {
	if r.RuntimeInspect != nil {
		return r.RuntimeInspect(ctx, oi, artifact)
	}
	d := oi.Spec.Sandbox.Daytona
	apiKey, err := r.secretValue(ctx, oi.Namespace, d.APIKeySecretRef)
	if err != nil {
		return provider.ArtifactState{}, fmt.Errorf("daytona api key: %w", err)
	}
	p, err := daytonaprovider.New(daytonaprovider.Config{APIKey: apiKey, APIURL: d.APIURL, Target: d.Target})
	if err != nil {
		return provider.ArtifactState{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.InspectBaseArtifact(ctx, artifact)
}

// runtimeJobInputsHash fingerprints everything the build Job consumes apart
// from the digest embedded in its name.
func (r *OpenInspectReconciler) runtimeJobInputsHash(oi *openinspectv1alpha1.OpenInspect) string {
	d := oi.Spec.Sandbox.Daytona
	override := ""
	if rt := oi.Spec.Sandbox.Runtime; rt != nil {
		override = rt.Image
	}
	h := sha256.New()
	for _, part := range []string{d.APIURL, d.Target, override, d.APIKeySecretRef.Name, d.APIKeySecretRef.Key, r.RuntimeBuilderImage} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// createRuntimeBuildJob creates the owned Job that performs the remote
// build. The pod mounts only the selected provider's Secret and no service
// account token; the runtime bundle itself is embedded in the builder image.
func (r *OpenInspectReconciler) createRuntimeBuildJob(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, jobName, artifactName string) error {
	if r.RuntimeBuilderImage == "" {
		return fmt.Errorf("runtime builder image is not configured (set --runtime-builder-image or RELATED_IMAGE_RUNTIME_BUILDER)")
	}
	d := oi.Spec.Sandbox.Daytona
	env := []corev1.EnvVar{
		{Name: "OI_PROVIDER", Value: providerDaytona},
		{Name: "OI_ARTIFACT_NAME", Value: artifactName},
		{Name: "DAYTONA_API_URL", Value: d.APIURL},
	}
	if d.Target != "" {
		env = append(env, corev1.EnvVar{Name: "DAYTONA_TARGET", Value: d.Target})
	}
	if rt := oi.Spec.Sandbox.Runtime; rt != nil && rt.Image != "" {
		env = append(env, corev1.EnvVar{Name: "OI_RUNTIME_BASE_IMAGE", Value: rt.Image})
	}
	env = append(env, corev1.EnvVar{
		Name: "DAYTONA_API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: d.APIKeySecretRef.Name},
			Key:                  d.APIKeySecretRef.Key,
		}},
	})

	backoff := int32(3)
	ttl := int32(3600)
	deadline := int64(3600)
	noToken := false
	runAsNonRoot, runAsUser := true, int64(65532)
	noPrivEsc, roRootFS := false, true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: oi.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "openinspect-runtime-build",
				"app.kubernetes.io/instance":   oi.Name,
				"app.kubernetes.io/managed-by": "openinspect-operator",
			},
			Annotations: map[string]string{
				runtimeInputsHashAnnotation: r.runtimeJobInputsHash(oi),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":     "openinspect-runtime-build",
					"app.kubernetes.io/instance": oi.Name,
				}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &noToken,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &runAsNonRoot,
						RunAsUser:      &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    runtimeBuilderContainer,
						Image:   r.RuntimeBuilderImage,
						Command: []string{"/runtime-builder"},
						Env:     env,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPrivEsc,
							ReadOnlyRootFilesystem:   &roRootFS,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						// Materialize() writes the runtime tree to /tmp.
						VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "tmp",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(oi, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

func jobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobCompletionTime(job *batchv1.Job) *metav1.Time {
	return job.Status.CompletionTime
}

func jobFailureTime(job *batchv1.Job) *metav1.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			t := c.LastTransitionTime
			return &t
		}
	}
	return nil
}

func jobFailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return "unknown failure"
}
