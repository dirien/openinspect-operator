package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
	modalprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/modal"
)

// modalDeployRetryInterval mirrors runtimeBuildRetryInterval for the
// modal-deploy Job.
const modalDeployRetryInterval = 10 * time.Minute

// GitHub App bindings on the control-plane worker that the upstream Modal
// app also needs (as its github-app secret).
var modalGitHubAppBindings = []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_INSTALLATION_ID"}

// ensureProviderApp reconciles the provider-side application. Only Modal has
// one (the upstream Modal app, deployed by the operator-owned modal-deploy
// Job, D5); for Daytona the condition is trivially True.
func (r *OpenInspectReconciler) ensureProviderApp(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) (pending bool, err error) {
	sb := oi.Spec.Sandbox
	switch {
	case sb == nil:
		oi.Status.ModalApp = nil
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionProviderApp)
		return false, nil
	case sb.Provider == providerDaytona:
		oi.Status.ModalApp = nil
		r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionTrue,
			"NoProviderApp", "provider has no deployed application")
		return false, nil
	case sb.Provider != providerModal:
		oi.Status.ModalApp = nil
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionProviderApp)
		return false, nil
	}
	return r.ensureModalApp(ctx, oi, cf)
}

// ensureModalApp converges the three named Modal secrets the unmodified
// upstream app requires, runs the modal-deploy Job when the pinned source or
// the secret inputs changed, and discovers + health-checks the deployed
// endpoints into status.modalApp.
func (r *OpenInspectReconciler) ensureModalApp(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) (pending bool, err error) {
	m := oi.Spec.Sandbox.Modal
	tokenID, err := r.secretValue(ctx, oi.Namespace, m.TokenIDSecretRef)
	if err != nil {
		return false, fmt.Errorf("modal token id: %w", err)
	}
	tokenSecret, err := r.secretValue(ctx, oi.Namespace, m.TokenSecretSecretRef)
	if err != nil {
		return false, fmt.Errorf("modal token secret: %w", err)
	}

	secrets, err := r.modalSecretValues(ctx, oi, cf)
	if err != nil {
		return false, err
	}
	// Keyed with the Modal token secret so the hash published in status
	// cannot be dictionary-attacked to recover low-entropy values.
	secretsHash := hashModalSecrets(secrets, tokenSecret)

	st := oi.Status.ModalApp
	if st == nil {
		st = &openinspectv1alpha1.ModalAppStatus{}
		oi.Status.ModalApp = st
	}
	if st.SecretsHash != secretsHash {
		ensure := r.ModalSecretsEnsure
		if ensure == nil {
			ensure = func(ctx context.Context, cfg modalprovider.Config, values map[string]map[string]string) error {
				p, err := modalprovider.New(cfg)
				if err != nil {
					return err
				}
				return p.EnsureNamedSecrets(ctx, values)
			}
		}
		if err := ensure(ctx, modalprovider.Config{
			TokenID: tokenID, TokenSecret: tokenSecret,
			Workspace: m.Workspace, Environment: modalEnvironment(m),
		}, secrets); err != nil {
			return false, fmt.Errorf("converge modal secrets: %w", err)
		}
		st.SecretsHash = secretsHash
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalSecretsConverged",
			"converged %d named modal secrets", len(secrets))
		// A secrets change redeploys the app (mirrors upstream's Terraform
		// trigger): the deploy resolves the named secrets.
		st.DeployedSourceRevision = ""
	}

	jobName := modalDeployJobName(oi, secretsHash)
	deployed := st.DeployedSourceRevision == sandboxruntime.UpstreamCommit()
	if !deployed {
		return r.modalDeployStep(ctx, oi, st, jobName)
	}

	// Converged: verify the app is still there and healthy (drift check).
	endpoints, err := r.discoverModal(ctx, oi, tokenID, tokenSecret)
	switch {
	case errors.Is(err, modalprovider.ErrAppNotFound):
		// The app was stopped or deleted out of band: redeploy.
		st.DeployedSourceRevision = ""
		r.Recorder.Event(oi, corev1.EventTypeWarning, "ModalAppMissing",
			"deployed modal app disappeared; redeploying")
		return r.modalDeployStep(ctx, oi, st, jobName)
	case err != nil:
		return false, err
	}
	st.AppID = endpoints.AppID
	st.HealthURL = endpoints.HealthURL
	st.WebSuffix = endpoints.WebSuffix
	if err := r.modalHealth(ctx, endpoints.HealthURL); err != nil {
		r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse,
			"AppUnhealthy", err.Error())
		return true, nil
	}
	r.setModalReady(oi)
	return false, nil
}

// modalDeployStep drives the modal-deploy Job to completion and, on success,
// discovers and health-checks the deployed endpoints before recording the
// deployment in status.
func (r *OpenInspectReconciler) modalDeployStep(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, st *openinspectv1alpha1.ModalAppStatus, jobName string) (pending bool, err error) {
	m := oi.Spec.Sandbox.Modal
	job := &batchv1.Job{}
	jobErr := r.Get(ctx, types.NamespacedName{Namespace: oi.Namespace, Name: jobName}, job)
	if jobErr != nil && client.IgnoreNotFound(jobErr) != nil {
		return false, jobErr
	}
	if jobErr != nil { // not found
		if err := r.createModalDeployJob(ctx, oi, jobName); err != nil {
			return true, err
		}
		r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse,
			"ModalDeployPending", fmt.Sprintf("deploy job %s created", jobName))
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalDeployStarted",
			"deploying upstream modal app (source %s)", sandboxruntime.UpstreamCommit())
		return true, nil
	}

	switch {
	case jobSucceeded(job):
		tokenID, err := r.secretValue(ctx, oi.Namespace, m.TokenIDSecretRef)
		if err != nil {
			return true, err
		}
		tokenSecret, err := r.secretValue(ctx, oi.Namespace, m.TokenSecretSecretRef)
		if err != nil {
			return true, err
		}
		endpoints, err := r.discoverModal(ctx, oi, tokenID, tokenSecret)
		if errors.Is(err, modalprovider.ErrAppNotFound) {
			// The deploy Job claims success but the app is not resolvable
			// (or was stopped after it finished). Surface it AND delete the
			// stale Job, otherwise its success would block the redeploy
			// until the Job's TTL expires.
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && client.IgnoreNotFound(err) != nil {
				return true, err
			}
			r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse,
				"ModalAppMissing", "deploy job succeeded but the app is not resolvable; redeploying")
			return true, nil
		}
		if err != nil {
			return true, err
		}
		if err := r.modalHealth(ctx, endpoints.HealthURL); err != nil {
			r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse,
				"AppUnhealthy", err.Error())
			return true, nil
		}
		st.AppID = endpoints.AppID
		st.HealthURL = endpoints.HealthURL
		st.WebSuffix = endpoints.WebSuffix
		st.DeployedSourceRevision = sandboxruntime.UpstreamCommit()
		if t := jobCompletionTime(job); t != nil {
			st.LastDeployTime = t
		} else {
			now := metav1.Now()
			st.LastDeployTime = &now
		}
		r.setModalReady(oi)
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalDeployed",
			"upstream modal app deployed and healthy at %s", endpoints.HealthURL)
		return false, nil
	case jobFailed(job):
		msg := jobFailureMessage(job)
		r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse, "ModalDeployFailed",
			fmt.Sprintf("deploy job %s failed: %s (retrying after %s)", jobName, msg, modalDeployRetryInterval))
		if t := jobFailureTime(job); t != nil && time.Since(t.Time) > modalDeployRetryInterval {
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && client.IgnoreNotFound(err) != nil {
				return true, err
			}
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalDeployRetried",
				"recreating failed deploy job %s", jobName)
		}
		return true, nil
	default:
		r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionFalse,
			"ModalDeploying", fmt.Sprintf("deploy job %s in progress", jobName))
		return true, nil
	}
}

// setModalReady marks both app and runtime conditions: for Modal the base
// image is defined and built by the deployed upstream app, so a healthy
// deployment at the pinned revision IS the runtime artifact.
func (r *OpenInspectReconciler) setModalReady(oi *openinspectv1alpha1.OpenInspect) {
	r.setCondition(oi, openinspectv1alpha1.ConditionProviderApp, metav1.ConditionTrue,
		"Reconciled", "upstream modal app deployed and healthy")
	r.setCondition(oi, openinspectv1alpha1.ConditionRuntime, metav1.ConditionTrue,
		"DeployedApp", "base image is defined and built by the deployed upstream Modal app")
}

func (r *OpenInspectReconciler) discoverModal(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, tokenID, tokenSecret string) (modalprovider.Endpoints, error) {
	if r.ModalDiscover != nil {
		return r.ModalDiscover(ctx, oi)
	}
	m := oi.Spec.Sandbox.Modal
	p, err := modalprovider.New(modalprovider.Config{
		TokenID: tokenID, TokenSecret: tokenSecret,
		Workspace: m.Workspace, Environment: modalEnvironment(m),
	})
	if err != nil {
		return modalprovider.Endpoints{}, err
	}
	return p.DiscoverEndpoints(ctx)
}

func (r *OpenInspectReconciler) modalHealth(ctx context.Context, url string) error {
	if r.ModalHealthCheck != nil {
		return r.ModalHealthCheck(ctx, url)
	}
	return modalprovider.HealthCheck(ctx, url)
}

// modalSecretValues assembles the three named Modal secrets the unmodified
// upstream app resolves at deploy time, entirely from values the CR already
// references — no new spec surface. validateSpec guarantees the referenced
// bindings exist for provider modal.
func (r *OpenInspectReconciler) modalSecretValues(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) (map[string]map[string]string, error) {
	anthropic, err := r.appSecretValue(ctx, oi, "ANTHROPIC_API_KEY")
	if err != nil {
		return nil, err
	}
	github := map[string]string{}
	for _, binding := range modalGitHubAppBindings {
		v, err := r.workerSecretValue(ctx, oi, binding)
		if err != nil {
			return nil, err
		}
		github[binding] = v
	}
	apiSecret, err := r.secretValue(ctx, oi.Namespace, oi.Spec.Sandbox.Modal.APISecretSecretRef)
	if err != nil {
		return nil, fmt.Errorf("modal api secret: %w", err)
	}
	ics, err := r.internalCallbackSecret(ctx, oi)
	if err != nil {
		return nil, err
	}
	host, err := r.controlPlaneHost(ctx, oi, cf)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]string{
		modalprovider.SecretLLMAPIKeys: {"ANTHROPIC_API_KEY": anthropic},
		modalprovider.SecretGitHubApp:  github,
		modalprovider.SecretInternalAPI: {
			"MODAL_API_SECRET":            apiSecret,
			"INTERNAL_CALLBACK_SECRET":    ics,
			"ALLOWED_CONTROL_PLANE_HOSTS": host,
			"CONTROL_PLANE_URL":           "https://" + host,
		},
	}, nil
}

// controlPlaneHost derives the workers.dev host of the control-plane worker,
// honoring the AppSecretsURL test hook's host when set.
func (r *OpenInspectReconciler) controlPlaneHost(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) (string, error) {
	subdomain, err := cf.AccountSubdomain(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%s.workers.dev", oi.ResourceName(controlPlaneWorkerName(oi)), subdomain), nil
}

// appSecretValue resolves one spec.appSecrets entry's backing value.
func (r *OpenInspectReconciler) appSecretValue(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, key string) (string, error) {
	for _, s := range oi.Spec.AppSecrets {
		if s.Key == key {
			return r.secretValue(ctx, oi.Namespace, s.SecretRef)
		}
	}
	return "", fmt.Errorf("spec.appSecrets has no %s entry", key)
}

// workerSecretValue resolves a secret binding declared on the control-plane
// worker.
func (r *OpenInspectReconciler) workerSecretValue(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, binding string) (string, error) {
	name := controlPlaneWorkerName(oi)
	for _, w := range oi.Spec.Workers {
		if w.Name != name {
			continue
		}
		for _, s := range w.Secrets {
			if s.Binding == binding {
				return r.secretValue(ctx, oi.Namespace, s.SecretRef)
			}
		}
	}
	return "", fmt.Errorf("control-plane worker %q has no %s secret binding", name, binding)
}

// hashModalSecrets fingerprints the desired named-secret set, HMAC-keyed so
// the published hash cannot be reversed.
func hashModalSecrets(secrets map[string]map[string]string, key string) string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	h := hmac.New(sha256.New, []byte(key))
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		keys := make([]string, 0, len(secrets[name]))
		for k := range secrets[name] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(secrets[name][k]))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// modalDeployJobName embeds the deploy inputs so a changed pin or secret set
// is a new Job, within the 63-character name limit.
func modalDeployJobName(oi *openinspectv1alpha1.OpenInspect, secretsHash string) string {
	h := sha256.New()
	h.Write([]byte(sandboxruntime.UpstreamCommit()))
	h.Write([]byte{0})
	h.Write([]byte(secretsHash))
	suffix := hex.EncodeToString(h.Sum(nil))[:8]
	base := oi.Name
	if limit := 63 - len("-mdp-") - len(suffix); len(base) > limit {
		base = base[:limit]
	}
	return base + "-mdp-" + suffix
}

// createModalDeployJob creates the owned Job that runs the pinned Modal CLI
// against the pinned upstream source (both baked into the deploy image).
// Only the Modal token Secret is exposed; the pod gets no service-account
// token.
func (r *OpenInspectReconciler) createModalDeployJob(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, jobName string) error {
	if r.ModalDeployImage == "" {
		return fmt.Errorf("modal deploy image is not configured (set --modal-deploy-image or RELATED_IMAGE_MODAL_DEPLOY)")
	}
	m := oi.Spec.Sandbox.Modal
	env := []corev1.EnvVar{
		{Name: "MODAL_ENVIRONMENT", Value: modalEnvironment(m)},
		{Name: "MODAL_TOKEN_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: m.TokenIDSecretRef.Name},
			Key:                  m.TokenIDSecretRef.Key,
		}}},
		{Name: "MODAL_TOKEN_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: m.TokenSecretSecretRef.Name},
			Key:                  m.TokenSecretSecretRef.Key,
		}}},
	}

	backoff := int32(3)
	ttl := int32(3600)
	deadline := int64(1800)
	noToken := false
	runAsNonRoot, runAsUser := true, int64(65532)
	noPrivEsc := false
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: oi.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "openinspect-modal-deploy",
				"app.kubernetes.io/instance":   oi.Name,
				"app.kubernetes.io/managed-by": "openinspect-operator",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":     "openinspect-modal-deploy",
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
						Name:  "modal-deploy",
						Image: r.ModalDeployImage,
						Env:   env,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noPrivEsc,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						// The Modal CLI writes build state under /tmp.
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
