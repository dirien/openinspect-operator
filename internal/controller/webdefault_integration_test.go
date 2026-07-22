package controller

import (
	"context"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	"github.com/dirien/openinspect-operator/internal/cfmock"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

// The default web image: with spec.web.image empty the operator deploys the
// release”'s provider-specific web image and wires the control-plane URLs in
// as runtime env — no per-deployment image builds, and a rename just rolls
// out new env.
var _ = Describe("Default web image", func() {
	const name = "web-default"
	ns := "default"
	key := types.NamespacedName{Name: name, Namespace: ns}
	var (
		mock       *httptest.Server
		reconciler *OpenInspectReconciler
	)

	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	spec := func(image string, env map[string]string) openinspectv1alpha1.OpenInspectSpec {
		return openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "webdef",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-web",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "wd-cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			Workers: []openinspectv1alpha1.WorkerSpec{{Name: "control-plane"}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "daytona",
				Daytona: &openinspectv1alpha1.DaytonaSandboxSpec{
					BaseSnapshot:    "ext-base",
					APIURL:          "https://daytona.invalid/api",
					APIKeySecretRef: openinspectv1alpha1.SecretKeyRef{Name: "wd-secrets", Key: "daytonaApiKey"},
				},
			},
			Web: &openinspectv1alpha1.WebSpec{Image: image, Env: env},
		}
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		reconciler = &OpenInspectReconciler{
			Client:          k8sClient,
			Scheme:          scheme.Scheme,
			Recorder:        record.NewFakeRecorder(100),
			WebImageDaytona: "ghcr.io/example/openinspect-web:test-daytona",
			RuntimeInspect: func(_ context.Context, _ *openinspectv1alpha1.OpenInspect, _ provider.Artifact) (provider.ArtifactState, error) {
				return provider.ArtifactState{Phase: provider.ArtifactReady}, nil
			},
			RuntimeDelete: func(_ context.Context, _ *openinspectv1alpha1.OpenInspect, _ provider.Artifact) error { return nil },
		}
		objs := []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wd-cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wd-secrets", Namespace: ns},
				StringData: map[string]string{"daytonaApiKey": "dtn"}},
		}
		for _, obj := range objs {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
	})

	AfterEach(func() {
		oi := &openinspectv1alpha1.OpenInspect{}
		if err := k8sClient.Get(ctx, key, oi); err == nil {
			oi.Finalizers = nil
			_ = k8sClient.Update(ctx, oi)
			_ = k8sClient.Delete(ctx, oi)
		}
	})

	webDeploy := func() *appsv1.Deployment {
		d := &appsv1.Deployment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, d)).To(Succeed())
		return d
	}

	It("deploys the release image with runtime-wired URLs and lets explicit env win", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec("", map[string]string{"CONTROL_PLANE_URL": "https://user-override.example"}),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		d := webDeploy()
		c := d.Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal("ghcr.io/example/openinspect-web:test-daytona"))
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		// Derived from the worker name + cfmock account subdomain.
		Expect(env["OI_WS_URL"]).To(Equal("wss://open-inspect-control-plane-webdef.cfmock.workers.dev"))
		Expect(env["CONTROL_PLANE_URL"]).To(Equal("https://user-override.example"), "explicit env must win")
		// The release image rewrites its bundles at start: image user + writable root.
		Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeFalse()))
		Expect(d.Spec.Template.Spec.SecurityContext.RunAsUser).To(BeNil())
		Expect(d.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
	})

	It("keeps the hardened defaults for user-supplied images", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec("ghcr.io/example/custom-web:v1", nil),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		d := webDeploy()
		c := d.Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal("ghcr.io/example/custom-web:v1"))
		Expect(c.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
		Expect(d.Spec.Template.Spec.SecurityContext.RunAsUser).To(HaveValue(Equal(int64(1000))))
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		Expect(env["OI_WS_URL"]).To(Equal("wss://open-inspect-control-plane-webdef.cfmock.workers.dev"),
			"URLs are still injected for custom images (harmless, explicit env wins)")
	})

	It("rejects an empty image without a managed provider as InvalidSpec", func() {
		s := spec("", nil)
		s.Sandbox = nil
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s,
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		got := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(cond.Reason).To(Equal("InvalidSpec"))
		Expect(cond.Message).To(ContainSubstring("spec.web.image is required"))
	})
})
