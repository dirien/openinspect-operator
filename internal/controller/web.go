package controller

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
)

// ensureWeb runs the web UI in-cluster as an owned Deployment + Service.
// Owner references make cleanup automatic on CR deletion; removal from spec
// is handled explicitly below. With no explicit image, the release's web
// image for the CR's provider is used and the operator wires the
// deployment-specific control-plane URLs into it at runtime — a rename
// rolls out new env instead of requiring an image rebuild.
func (r *OpenInspectReconciler) ensureWeb(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) error {
	name := oi.Name + "-web"
	if oi.Spec.Web == nil {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}},
		} {
			if err := r.deleteOwned(ctx, oi, obj); err != nil {
				return fmt.Errorf("remove web %T: %w", obj, err)
			}
		}
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionWeb)
		return nil
	}
	web := oi.Spec.Web
	labels := map[string]string{
		"app.kubernetes.io/name":       "openinspect-web",
		"app.kubernetes.io/instance":   oi.Name,
		"app.kubernetes.io/managed-by": "openinspect-operator",
	}
	port := web.Port
	if port == 0 {
		port = 3000
	}

	image := web.Image
	// The release image's entrypoint rewrites the baked WebSocket-URL
	// placeholder in place at startup, which needs (a) the image's own user
	// (files are owned by uid 1001) and (b) a writable root filesystem.
	// User-supplied images keep the stricter defaults.
	defaulted := image == ""
	if defaulted {
		var err error
		if image, err = r.defaultWebImage(oi); err != nil {
			return err
		}
	}

	// The operator knows the control-plane URLs; inject them so the
	// default image needs zero URL configuration. Explicit spec env wins.
	injected := map[string]string{}
	if sb := oi.Spec.Sandbox; sb != nil && (sb.Provider == providerModal || sb.Provider == providerDaytona) {
		host, err := r.controlPlaneHost(ctx, oi, cf)
		if err != nil {
			return fmt.Errorf("derive control-plane host: %w", err)
		}
		injected["OI_WS_URL"] = "wss://" + host
		injected["CONTROL_PLANE_URL"] = "https://" + host
	}
	explicit := map[string]bool{}
	for k := range web.Env {
		explicit[k] = true
	}
	for _, s := range web.SecretEnv {
		explicit[s.Binding] = true
	}

	env := make([]corev1.EnvVar, 0, len(injected)+len(web.Env)+len(web.SecretEnv))
	for _, k := range sortedKeys(injected) {
		if !explicit[k] {
			env = append(env, corev1.EnvVar{Name: k, Value: injected[k]})
		}
	}
	for _, k := range sortedKeys(web.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: web.Env[k]})
	}
	for _, s := range web.SecretEnv {
		env = append(env, corev1.EnvVar{
			Name: s.Binding,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretRef.Name},
					Key:                  s.SecretRef.Key,
				},
			},
		})
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = web.Replicas
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Template.Labels = labels
		runAsNonRoot, runAsUser := true, int64(1000)
		noPrivEsc := false
		roRootFS := !defaulted
		podSC := &corev1.PodSecurityContext{
			RunAsNonRoot:   &runAsNonRoot,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		if !defaulted {
			// Arbitrary user images get a forced non-root uid; the release
			// image declares its own user, which must own its files for the
			// startup rewrite.
			podSC.RunAsUser = &runAsUser
		}
		deploy.Spec.Template.Spec.SecurityContext = podSC
		// The read-only root filesystem needs a writable /tmp for typical web
		// runtimes (Next.js et al.).
		deploy.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		// TCP, not HTTP GET /: the image is arbitrary and its root path may
		// 404, require auth, or not speak HTTP at all — an HTTP liveness
		// probe would restart-loop such a container. A listening socket is
		// the only universally safe signal.
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
		}
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "web",
			Image: image,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:   env,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noPrivEsc,
				ReadOnlyRootFilesystem:   &roRootFS,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts:   []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
			ReadinessProbe: probe,
			LivenessProbe: &corev1.Probe{
				ProbeHandler:     probe.ProbeHandler,
				FailureThreshold: 6,
				PeriodSeconds:    20,
			},
		}}
		return ctrl.SetControllerReference(oi, deploy, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("web deployment: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "http", Port: 80, TargetPort: intstr.FromInt32(port),
		}}
		return ctrl.SetControllerReference(oi, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("web service: %w", err)
	}

	ready := deploy.Status.ReadyReplicas
	r.setCondition(oi, openinspectv1alpha1.ConditionWeb, metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("web deployment applied, %d replicas ready", ready))
	return nil
}

// deleteOwned removes obj only if this CR is its controller owner, so a name
// collision with an unrelated object is never destructive.
func (r *OpenInspectReconciler) deleteOwned(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, oi) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// defaultWebImage resolves the release's web image for the CR's provider.
// One variant exists per managed provider because the provider is baked
// into the client bundle by upstream's design; the deployment-specific
// WebSocket URL is wired at runtime by the image's entrypoint.
func (r *OpenInspectReconciler) defaultWebImage(oi *openinspectv1alpha1.OpenInspect) (string, error) {
	sb := oi.Spec.Sandbox
	provider := ""
	if sb != nil {
		provider = sb.Provider
	}
	switch provider {
	case providerModal:
		if r.WebImageModal == "" {
			return "", fmt.Errorf("no default web image configured for provider modal (set --web-image-modal / RELATED_IMAGE_WEB_MODAL) — or set spec.web.image")
		}
		return r.WebImageModal, nil
	case providerDaytona:
		if r.WebImageDaytona == "" {
			return "", fmt.Errorf("no default web image configured for provider daytona (set --web-image-daytona / RELATED_IMAGE_WEB_DAYTONA) — or set spec.web.image")
		}
		return r.WebImageDaytona, nil
	default:
		return "", fmt.Errorf("spec.web.image is required: release web images exist only for the managed providers (modal, daytona), not %q", provider)
	}
}
