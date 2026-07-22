package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	"github.com/dirien/openinspect-operator/internal/cfmock"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
)

var _ = Describe("Provider sweep on deletion", func() {
	ns := "default"
	var (
		mock       *httptest.Server
		reconciler *OpenInspectReconciler
	)

	reconcile := func(key types.NamespacedName) error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	insertSession := func(account, dbID, sessionID string) {
		body := fmt.Sprintf(`{"sql":"INSERT INTO sessions (id) VALUES ('%s')"}`, sessionID)
		req, err := http.NewRequest(http.MethodPost,
			mock.URL+"/accounts/"+account+"/d1/database/"+dbID+"/query", strings.NewReader(body))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		ExpectWithOffset(1, resp.StatusCode).To(Equal(http.StatusOK))
	}

	deleteAndFinalize := func(key types.NamespacedName) {
		oi := &openinspectv1alpha1.OpenInspect{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, oi)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Delete(ctx, oi)).To(Succeed())
		ExpectWithOffset(1, reconcile(key)).To(Succeed())
		err := k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{})
		ExpectWithOffset(1, errors.IsNotFound(err)).To(BeTrue(), "CR should be finalized and gone")
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		store := &fakeSecretsAPI{secret: "ics-test", store: map[string]string{}}
		storeServer := httptest.NewServer(store.handler(GinkgoT()))
		DeferCleanup(storeServer.Close)
		reconciler = &OpenInspectReconciler{
			Client:   k8sClient,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(100),
			// This suite covers deletion sweeps; the provider-app flow has
			// its own Describe block.
			ProviderAppEnsure: func(context.Context, *openinspectv1alpha1.OpenInspect, *cfclient.Client) (bool, error) {
				return false, nil
			},
			AppSecretsURL: func(*openinspectv1alpha1.OpenInspect) string {
				return storeServer.URL + "/secrets"
			},
		}

		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ps-cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ps-secrets", Namespace: ns},
				StringData: map[string]string{
					"daytonaApiKey":    "dtn-test",
					"modalTokenId":     "ak-test",
					"modalTokenSecret": "as-test",
					"modalApiSecret":   "api-test",
					"ics":              "ics-test",
					"ghAppId":          "1",
					"ghAppKey":         "rsa",
					"ghAppInstall":     "2",
					"anthropic":        "sk-ant-test",
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ps-migrations", Namespace: ns},
				Data:       map[string]string{"0001_init.sql": "CREATE TABLE sessions (id TEXT PRIMARY KEY);"},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ps-bundles", Namespace: ns},
				Data:       map[string]string{"cp.js": "export default { fetch() { return new Response('cp') } }"},
			},
		}
		for _, obj := range objs {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
	})

	daytonaCR := func(name, deploymentName, account string) *openinspectv1alpha1.OpenInspect {
		return &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: deploymentName,
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         account,
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "ps-cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				D1: &openinspectv1alpha1.D1Spec{MigrationsConfigMapRef: "ps-migrations"},
				Workers: []openinspectv1alpha1.WorkerSpec{{
					Name:     "control-plane",
					Script:   &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "ps-bundles", Key: "cp.js"}},
					Bindings: openinspectv1alpha1.WorkerBindings{D1: []openinspectv1alpha1.D1Binding{{Binding: "DB"}}},
				}},
				Sandbox: &openinspectv1alpha1.SandboxSpec{
					Provider: "daytona",
					Daytona: &openinspectv1alpha1.DaytonaSandboxSpec{
						BaseSnapshot:    "base",
						APIURL:          "https://daytona.test/api",
						APIKeySecretRef: openinspectv1alpha1.SecretKeyRef{Name: "ps-secrets", Key: "daytonaApiKey"},
					},
				},
			},
		}
	}

	It("sweeps exactly the D1-owned Daytona sandboxes on delete", func() {
		oi := daytonaCR("ps-daytona", "psd", "acc-psd")
		key := types.NamespacedName{Name: oi.Name, Namespace: ns}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile(key)).To(Succeed())

		got := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.D1DatabaseID).NotTo(BeEmpty())
		insertSession("acc-psd", got.Status.D1DatabaseID, "sess-1")

		var capturedOwned map[string]struct{}
		calls := 0
		reconciler.DaytonaCleanup = func(_ context.Context, d *openinspectv1alpha1.DaytonaSandboxSpec, apiKey string, owned map[string]struct{}) (int, error) {
			calls++
			capturedOwned = owned
			Expect(d.APIURL).To(Equal("https://daytona.test/api"))
			Expect(apiKey).To(Equal("dtn-test"))
			return len(owned), nil
		}

		deleteAndFinalize(key)
		Expect(calls).To(Equal(1))
		Expect(capturedOwned).To(HaveKey("sess-1"))
		Expect(capturedOwned).To(HaveLen(1))
	})

	It("does not wedge deletion when the D1 database is already gone", func() {
		oi := daytonaCR("ps-daytona-wedge", "psw", "acc-psw")
		key := types.NamespacedName{Name: oi.Name, Namespace: ns}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile(key)).To(Succeed())

		got := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.D1DatabaseID).NotTo(BeEmpty())

		// Delete the database out of band: ownership evidence is gone, so the
		// sweep must be skipped instead of blocking the finalizer forever.
		cfc := cfclient.New("acc-psw", "test-token", mock.URL)
		Expect(cfc.DeleteD1(ctx, got.Status.D1DatabaseID)).To(Succeed())

		calls := 0
		reconciler.DaytonaCleanup = func(context.Context, *openinspectv1alpha1.DaytonaSandboxSpec, string, map[string]struct{}) (int, error) {
			calls++
			return 0, nil
		}

		deleteAndFinalize(key)
		Expect(calls).To(BeZero(), "sweep must be skipped without ownership evidence")
	})

	It("finalizes a Modal CR whose app is not found without claiming a stop", func() {
		oi := daytonaCR("ps-modal", "psm", "acc-psm")
		oi.Spec.Sandbox = &openinspectv1alpha1.SandboxSpec{
			Provider: "modal",
			Modal: &openinspectv1alpha1.ModalSandboxSpec{
				Workspace:            "ws",
				TokenIDSecretRef:     openinspectv1alpha1.SecretKeyRef{Name: "ps-secrets", Key: "modalTokenId"},
				TokenSecretSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "ps-secrets", Key: "modalTokenSecret"},
				APISecretSecretRef:   openinspectv1alpha1.SecretKeyRef{Name: "ps-secrets", Key: "modalApiSecret"},
			},
		}
		// Provider modal requires the upstream app inputs to pass validation.
		ref := func(key string) openinspectv1alpha1.SecretKeyRef {
			return openinspectv1alpha1.SecretKeyRef{Name: "ps-secrets", Key: key}
		}
		oi.Spec.Workers[0].Secrets = []openinspectv1alpha1.WorkerSecret{
			{Binding: "INTERNAL_CALLBACK_SECRET", SecretRef: ref("ics")},
			{Binding: "GITHUB_APP_ID", SecretRef: ref("ghAppId")},
			{Binding: "GITHUB_APP_PRIVATE_KEY", SecretRef: ref("ghAppKey")},
			{Binding: "GITHUB_APP_INSTALLATION_ID", SecretRef: ref("ghAppInstall")},
		}
		oi.Spec.AppSecrets = []openinspectv1alpha1.AppSecret{{Key: "ANTHROPIC_API_KEY", SecretRef: ref("anthropic")}}
		key := types.NamespacedName{Name: oi.Name, Namespace: ns}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile(key)).To(Succeed())

		recorder := record.NewFakeRecorder(100)
		reconciler.Recorder = recorder
		reconciler.ModalCleanup = func(context.Context, *openinspectv1alpha1.ModalSandboxSpec, string, string) (int, bool, error) {
			return 0, false, nil
		}

		deleteAndFinalize(key)
		var sawNotFound, sawStopped bool
		for len(recorder.Events) > 0 {
			e := <-recorder.Events
			if strings.Contains(e, "ModalAppNotFound") {
				sawNotFound = true
			}
			if strings.Contains(e, "ModalAppStopped") {
				sawStopped = true
			}
		}
		Expect(sawNotFound).To(BeTrue(), "must surface that no app was cleaned")
		Expect(sawStopped).To(BeFalse(), "must not claim a stop that never ran")
	})
})
