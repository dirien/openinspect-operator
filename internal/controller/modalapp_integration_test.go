package controller

import (
	"context"
	"fmt"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
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
	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
	modalprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/modal"
)

// The Modal provider-app flow: named secrets converge through the adapter,
// the modal-deploy Job runs the pinned CLI, and the discovered endpoints
// land in status.modalApp — including the web suffix the control-plane
// injection consumes. Modal itself is faked via the granular hooks; the Job
// status is set by the test like a job controller would.
var _ = Describe("Modal provider app", func() {
	const name = "modalapp-deploy"
	ns := "default"
	key := types.NamespacedName{Name: name, Namespace: ns}
	var (
		mock          *httptest.Server
		reconciler    *OpenInspectReconciler
		ensuredCalls  int
		ensuredValues map[string]map[string]string
		discover      func() (modalprovider.Endpoints, error)
		healthErr     error
	)

	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	getOI := func() *openinspectv1alpha1.OpenInspect {
		oi := &openinspectv1alpha1.OpenInspect{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, oi)).To(Succeed())
		return oi
	}

	modalAppSpec := func() openinspectv1alpha1.OpenInspectSpec {
		ref := func(key string) openinspectv1alpha1.SecretKeyRef {
			return openinspectv1alpha1.SecretKeyRef{Name: "ma-secrets", Key: key}
		}
		return openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "modalapp",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-123",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "ma-cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:   "control-plane",
				Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "ma-bundles", Key: "cp.js"}},
				Secrets: []openinspectv1alpha1.WorkerSecret{
					{Binding: "INTERNAL_CALLBACK_SECRET", SecretRef: ref("ics")},
					{Binding: "GITHUB_APP_ID", SecretRef: ref("ghAppId")},
					{Binding: "GITHUB_APP_PRIVATE_KEY", SecretRef: ref("ghAppKey")},
					{Binding: "GITHUB_APP_INSTALLATION_ID", SecretRef: ref("ghAppInstall")},
				},
			}},
			AppSecrets: []openinspectv1alpha1.AppSecret{{
				Key: "ANTHROPIC_API_KEY", SecretRef: ref("anthropic"),
			}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "modal",
				Modal: &openinspectv1alpha1.ModalSandboxSpec{
					Workspace:            "dirien",
					Environment:          "staging",
					TokenIDSecretRef:     ref("tokenId"),
					TokenSecretSecretRef: ref("tokenSecret"),
					APISecretSecretRef:   ref("apiSecret"),
				},
			},
		}
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		store := &fakeSecretsAPI{secret: "ics-val", store: map[string]string{}}
		storeServer := httptest.NewServer(store.handler(GinkgoT()))
		DeferCleanup(storeServer.Close)

		ensuredCalls = 0
		ensuredValues = nil
		healthErr = nil
		discover = func() (modalprovider.Endpoints, error) { //nolint:unparam // error varies per spec via reassignment
			return modalprovider.Endpoints{
				AppID:     "ap-123",
				HealthURL: "https://dirien-staging--open-inspect-api-health.modal.run",
				WebSuffix: "staging",
			}, nil
		}
		reconciler = &OpenInspectReconciler{
			Client:           k8sClient,
			Scheme:           scheme.Scheme,
			Recorder:         record.NewFakeRecorder(100),
			ModalDeployImage: "example.com/openinspect-modal-deploy:test",
			ModalSecretsEnsure: func(_ context.Context, cfg modalprovider.Config, values map[string]map[string]string) error {
				ensuredCalls++
				ensuredValues = values
				Expect(cfg.Environment).To(Equal("staging"))
				return nil
			},
			ModalDiscover: func(context.Context, *openinspectv1alpha1.OpenInspect) (modalprovider.Endpoints, error) {
				return discover()
			},
			ModalHealthCheck: func(context.Context, string) error { return healthErr },
			AppSecretsURL: func(*openinspectv1alpha1.OpenInspect) string {
				return storeServer.URL + "/secrets"
			},
		}
		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ma-cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ma-secrets", Namespace: ns},
				StringData: map[string]string{
					"ics": "ics-val", "ghAppId": "1", "ghAppKey": "rsa", "ghAppInstall": "2",
					"anthropic": "sk-ant", "tokenId": "ak-1", "tokenSecret": "as-1", "apiSecret": "internal-1",
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ma-bundles", Namespace: ns},
				Data:       map[string]string{"cp.js": "export default {}"},
			},
		}
		for _, obj := range objs {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
	})

	AfterEach(func() {
		oi := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		_ = k8sClient.Delete(ctx, oi)
		reconciler.ModalCleanup = func(context.Context, *openinspectv1alpha1.ModalSandboxSpec, string, string) (int, bool, error) {
			return 0, true, nil
		}
		_ = reconcile()
		// Last resort so one failed spec can never wedge the rest of the
		// suite behind a terminating CR.
		latest := &openinspectv1alpha1.OpenInspect{}
		if err := k8sClient.Get(ctx, key, latest); err == nil {
			latest.Finalizers = nil
			_ = k8sClient.Update(ctx, latest)
		}
		jobs := &batchv1.JobList{}
		_ = k8sClient.List(ctx, jobs, client.InNamespace(ns))
		for i := range jobs.Items {
			_ = k8sClient.Delete(ctx, &jobs.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground))
		}
	})

	completeJob := func(job *batchv1.Job) {
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	deployJob := func() *batchv1.Job {
		jobs := &batchv1.JobList{}
		ExpectWithOffset(1, k8sClient.List(ctx, jobs, client.InNamespace(ns),
			client.MatchingLabels{"app.kubernetes.io/name": "openinspect-modal-deploy"})).To(Succeed())
		ExpectWithOffset(1, jobs.Items).To(HaveLen(1))
		return &jobs.Items[0]
	}

	It("converges secrets, deploys through the Job, and records discovered endpoints", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       modalAppSpec(),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		By("named modal secrets converged from CR-referenced values only")
		Expect(ensuredCalls).To(Equal(1))
		Expect(ensuredValues).To(HaveKey(modalprovider.SecretLLMAPIKeys))
		Expect(ensuredValues[modalprovider.SecretLLMAPIKeys]).To(HaveKeyWithValue("ANTHROPIC_API_KEY", "sk-ant"))
		Expect(ensuredValues[modalprovider.SecretGitHubApp]).To(HaveKeyWithValue("GITHUB_APP_PRIVATE_KEY", "rsa"))
		internal := ensuredValues[modalprovider.SecretInternalAPI]
		Expect(internal).To(HaveKeyWithValue("MODAL_API_SECRET", "internal-1"))
		Expect(internal).To(HaveKeyWithValue("INTERNAL_CALLBACK_SECRET", "ics-val"))
		// Host derived from the cfmock account subdomain.
		Expect(internal).To(HaveKeyWithValue("ALLOWED_CONTROL_PLANE_HOSTS", "open-inspect-control-plane-modalapp.cfmock.workers.dev"))
		Expect(internal).To(HaveKeyWithValue("CONTROL_PLANE_URL", "https://open-inspect-control-plane-modalapp.cfmock.workers.dev"))

		By("deploy job created with only the Modal token secret")
		job := deployJob()
		Expect(metav1.IsControlledBy(job, getOI())).To(BeTrue())
		pod := job.Spec.Template.Spec
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.Containers[0].Image).To(Equal("example.com/openinspect-modal-deploy:test"))
		env := map[string]corev1.EnvVar{}
		for _, e := range pod.Containers[0].Env {
			env[e.Name] = e
		}
		Expect(env["MODAL_ENVIRONMENT"].Value).To(Equal("staging"))
		Expect(env["MODAL_TOKEN_ID"].ValueFrom.SecretKeyRef.Key).To(Equal("tokenId"))
		Expect(env["MODAL_TOKEN_SECRET"].ValueFrom.SecretKeyRef.Key).To(Equal("tokenSecret"))

		oiNow := getOI()
		cond := meta.FindStatusCondition(oiNow.Status.Conditions, openinspectv1alpha1.ConditionProviderApp)
		Expect(cond.Reason).To(Equal("ModalDeployPending"))
		Expect(meta.FindStatusCondition(oiNow.Status.Conditions, openinspectv1alpha1.ConditionReady).Reason).
			To(Equal("ProviderAppNotReady"))

		By("job completion records the deployment and discovered endpoints")
		completeJob(job)
		Expect(reconcile()).To(Succeed())
		oiNow = getOI()
		app := oiNow.Status.ModalApp
		Expect(app).NotTo(BeNil())
		Expect(app.AppID).To(Equal("ap-123"))
		Expect(app.HealthURL).To(Equal("https://dirien-staging--open-inspect-api-health.modal.run"))
		Expect(app.WebSuffix).To(Equal("staging"))
		Expect(app.DeployedSourceRevision).To(Equal(sandboxruntime.UpstreamCommit()))
		Expect(app.LastDeployTime).NotTo(BeNil())
		Expect(app.SecretsHash).NotTo(BeEmpty())
		Expect(meta.IsStatusConditionTrue(oiNow.Status.Conditions, openinspectv1alpha1.ConditionProviderApp)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(oiNow.Status.Conditions, openinspectv1alpha1.ConditionRuntime)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(oiNow.Status.Conditions, openinspectv1alpha1.ConditionReady)).To(BeTrue())

		By("the discovered web suffix reaches the control-plane worker")
		Expect(workerBindingText(mock.URL, "open-inspect-control-plane-modalapp", "MODAL_ENVIRONMENT_WEB_SUFFIX")).
			To(Equal("staging"))

		By("no re-deploy and no secret churn when nothing changed")
		Expect(reconcile()).To(Succeed())
		Expect(ensuredCalls).To(Equal(1))

		By("rotating a fed secret reconverges and redeploys")
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ma-secrets", Namespace: ns}, sec)).To(Succeed())
		sec.Data["anthropic"] = []byte("sk-ant-rotated")
		Expect(k8sClient.Update(ctx, sec)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(ensuredCalls).To(Equal(2))
		Expect(ensuredValues[modalprovider.SecretLLMAPIKeys]).To(HaveKeyWithValue("ANTHROPIC_API_KEY", "sk-ant-rotated"))
		oiNow = getOI()
		Expect(meta.FindStatusCondition(oiNow.Status.Conditions, openinspectv1alpha1.ConditionProviderApp).Reason).
			To(Equal("ModalDeployPending"), "a secrets change must redeploy the app")
	})

	It("redeploys when the app disappears out of band", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       modalAppSpec(),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		completeJob(deployJob())
		Expect(reconcile()).To(Succeed())
		Expect(meta.IsStatusConditionTrue(getOI().Status.Conditions, openinspectv1alpha1.ConditionProviderApp)).To(BeTrue())

		discover = func() (modalprovider.Endpoints, error) { //nolint:unparam // value varies per spec via reassignment
			return modalprovider.Endpoints{}, fmt.Errorf("wrapped: %w", modalprovider.ErrAppNotFound)
		}
		Expect(reconcile()).To(Succeed())
		oiNow := getOI()
		Expect(oiNow.Status.ModalApp.DeployedSourceRevision).To(BeEmpty(),
			"a vanished app must clear the deployed revision to force a redeploy")
		Expect(meta.IsStatusConditionTrue(oiNow.Status.Conditions, openinspectv1alpha1.ConditionReady)).To(BeFalse())
	})

	It("reports an unhealthy app without redeploying", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       modalAppSpec(),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		completeJob(deployJob())
		Expect(reconcile()).To(Succeed())

		healthErr = fmt.Errorf("health check: HTTP 503")
		Expect(reconcile()).To(Succeed())
		oiNow := getOI()
		cond := meta.FindStatusCondition(oiNow.Status.Conditions, openinspectv1alpha1.ConditionProviderApp)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("AppUnhealthy"))
		Expect(oiNow.Status.ModalApp.DeployedSourceRevision).To(Equal(sandboxruntime.UpstreamCommit()),
			"an unhealthy app is not redeployed — deployment would not fix it")
	})

	It("surfaces a failed deploy job", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       modalAppSpec(),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		job := deployJob()
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 4
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded", LastTransitionTime: now})
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		Expect(reconcile()).To(Succeed())
		cond := meta.FindStatusCondition(getOI().Status.Conditions, openinspectv1alpha1.ConditionProviderApp)
		Expect(cond.Reason).To(Equal("ModalDeployFailed"))
		Expect(cond.Message).To(ContainSubstring("BackoffLimitExceeded"))
	})

	It("rejects a modal spec without the upstream app inputs as InvalidSpec", func() {
		spec := modalAppSpec()
		spec.AppSecrets = nil
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		cond := meta.FindStatusCondition(getOI().Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(cond.Reason).To(Equal("InvalidSpec"))
		Expect(cond.Message).To(ContainSubstring("ANTHROPIC_API_KEY"))
	})
})
