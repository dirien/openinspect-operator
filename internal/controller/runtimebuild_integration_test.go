package controller

import (
	"context"
	"encoding/json"
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
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

// The managed-runtime flow: the controller creates a digest-named build Job,
// reflects its outcome (verified against the provider) into
// status.sandboxRuntime and RuntimeReady, and flips the control-plane
// injection to the built artifact. The provider is faked via the
// RuntimeInspect hook; the Job never runs (envtest has no kubelet), its
// status is set by the test like a job controller would.
var _ = Describe("Managed sandbox runtime", func() {
	const name = "runtime-deploy"
	ns := "default"
	key := types.NamespacedName{Name: name, Namespace: ns}
	var (
		mock       *httptest.Server
		reconciler *OpenInspectReconciler
		inspect    provider.ArtifactState
		inspectErr error
	)

	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	daytonaSpec := func(baseSnapshot string) openinspectv1alpha1.OpenInspectSpec {
		return openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "runtime",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-123",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:   "control-plane",
				Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
			}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "daytona",
				Daytona: &openinspectv1alpha1.DaytonaSandboxSpec{
					BaseSnapshot:    baseSnapshot,
					APIURL:          "https://daytona.invalid/api",
					APIKeySecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "daytonaApiKey"},
				},
			},
		}
	}

	getOI := func() *openinspectv1alpha1.OpenInspect {
		oi := &openinspectv1alpha1.OpenInspect{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, oi)).To(Succeed())
		return oi
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		inspect = provider.ArtifactState{Phase: provider.ArtifactAbsent}
		inspectErr = nil
		reconciler = &OpenInspectReconciler{
			Client:              k8sClient,
			Scheme:              scheme.Scheme,
			Recorder:            record.NewFakeRecorder(100),
			RuntimeBuilderImage: "example.com/openinspect-operator:test",
			RuntimeInspect: func(context.Context, *openinspectv1alpha1.OpenInspect, provider.Artifact) (provider.ArtifactState, error) {
				return inspect, inspectErr
			},
			// Finalization deletes managed artifacts; never let it reach the
			// real adapter (and its unresolvable test endpoint) from here.
			RuntimeDelete: func(context.Context, *openinspectv1alpha1.OpenInspect, provider.Artifact) error {
				return nil
			},
		}
		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-secrets", Namespace: ns},
				StringData: map[string]string{"daytonaApiKey": "dtn-hunter2"},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "bundles", Namespace: ns},
				Data:       map[string]string{"control-plane.js": "export default {}"},
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
		// Finalization needs a pass; deletionPolicy Delete against the mock
		// succeeds because provider cleanup is skipped without session
		// evidence.
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
			// Background propagation, not the Job API's legacy Orphan
			// default: orphaning parks the object behind a finalizer that
			// only the (absent) GC controller would clear.
			_ = k8sClient.Delete(ctx, &jobs.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground))
		}
	})

	It("builds the managed artifact through a Job and flips the injection when ready", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       daytonaSpec(""),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		oi = getOI()
		st := oi.Status.SandboxRuntime
		Expect(st).NotTo(BeNil())
		Expect(st.Phase).To(Equal(openinspectv1alpha1.RuntimePhasePending))
		// The active reference stays empty until the first build is
		// confirmed: nothing may advertise an artifact that does not exist.
		Expect(st.ArtifactName).To(BeEmpty())
		Expect(st.Digest).To(BeEmpty())

		cond := meta.FindStatusCondition(oi.Status.Conditions, openinspectv1alpha1.ConditionRuntime)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RuntimeBuildPending"))
		ready := meta.FindStatusCondition(oi.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal("RuntimeNotReady"))

		// The Job carries the artifact name, only the selected provider's
		// Secret, no service-account token, and the inputs hash.
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1))
		job := jobs.Items[0]
		Expect(metav1.IsControlledBy(&job, oi)).To(BeTrue())
		Expect(job.Annotations).To(HaveKey("openinspect.dirien.dev/inputs-hash"))
		pod := job.Spec.Template.Spec
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.Containers).To(HaveLen(1))
		Expect(pod.Containers[0].Image).To(Equal("example.com/openinspect-operator:test"))
		Expect(pod.Containers[0].Command).To(Equal([]string{"/runtime-builder"}))
		env := map[string]corev1.EnvVar{}
		for _, e := range pod.Containers[0].Env {
			env[e.Name] = e
		}
		Expect(env["OI_PROVIDER"].Value).To(Equal("daytona"))
		targetArtifact := env["OI_ARTIFACT_NAME"].Value
		Expect(targetArtifact).To(HavePrefix("oi-"))
		Expect(env["DAYTONA_API_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("cp-secrets"))

		// While pending, the control-plane worker must not advertise a
		// snapshot that does not exist yet.
		Expect(workerBindingNames(mock.URL, "open-inspect-control-plane-runtime")).NotTo(ContainElement("DAYTONA_BASE_SNAPSHOT"))

		// Job completes; the provider confirms the artifact.
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
		job.Status.Succeeded = 1
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
		inspect = provider.ArtifactState{Phase: provider.ArtifactReady, ID: "snap-id-1"}

		Expect(reconcile()).To(Succeed())
		oi = getOI()
		st = oi.Status.SandboxRuntime
		Expect(st.Phase).To(Equal(openinspectv1alpha1.RuntimePhaseReady))
		Expect(st.ArtifactName).To(Equal(targetArtifact))
		Expect(st.Digest).To(HavePrefix("sha256:"))
		Expect(st.SourceRevision).To(HaveLen(40))
		Expect(st.ArtifactID).To(Equal("snap-id-1"))
		Expect(st.LastBuildTime).NotTo(BeNil())
		Expect(meta.IsStatusConditionTrue(oi.Status.Conditions, openinspectv1alpha1.ConditionRuntime)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(oi.Status.Conditions, openinspectv1alpha1.ConditionReady)).To(BeTrue())

		// The built artifact is now injected into the control plane.
		Expect(workerBindingText(mock.URL, "open-inspect-control-plane-runtime", "DAYTONA_BASE_SNAPSHOT")).
			To(Equal(st.ArtifactName))
	})

	It("reports a failed build Job and keeps Ready false", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       daytonaSpec(""),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1))
		job := jobs.Items[0]
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded", LastTransitionTime: now})
		job.Status.Failed = 4
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

		Expect(reconcile()).To(Succeed())
		oi = getOI()
		Expect(oi.Status.SandboxRuntime.Phase).To(Equal(openinspectv1alpha1.RuntimePhaseFailed))
		cond := meta.FindStatusCondition(oi.Status.Conditions, openinspectv1alpha1.ConditionRuntime)
		Expect(cond.Reason).To(Equal("RuntimeBuildFailed"))
		Expect(cond.Message).To(ContainSubstring("BackoffLimitExceeded"))
		Expect(meta.IsStatusConditionTrue(oi.Status.Conditions, openinspectv1alpha1.ConditionReady)).To(BeFalse())
		// The failed Job is retained for the cool-down window.
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1))
	})

	It("uses an external snapshot as-is without building", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       daytonaSpec("prebuilt-base"),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		oi = getOI()
		st := oi.Status.SandboxRuntime
		Expect(st.Phase).To(Equal(openinspectv1alpha1.RuntimePhaseExternal))
		Expect(st.ArtifactName).To(Equal("prebuilt-base"))
		cond := meta.FindStatusCondition(oi.Status.Conditions, openinspectv1alpha1.ConditionRuntime)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("ExternalArtifact"))
		Expect(meta.IsStatusConditionTrue(oi.Status.Conditions, openinspectv1alpha1.ConditionReady)).To(BeTrue())

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
		Expect(workerBindingText(mock.URL, "open-inspect-control-plane-runtime", "DAYTONA_BASE_SNAPSHOT")).
			To(Equal("prebuilt-base"))
	})

	It("adopts an already-existing remote artifact without a Job", func() {
		inspect = provider.ArtifactState{Phase: provider.ArtifactReady, ID: "snap-pre"}
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       daytonaSpec(""),
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())

		oi = getOI()
		Expect(oi.Status.SandboxRuntime.Phase).To(Equal(openinspectv1alpha1.RuntimePhaseReady))
		Expect(oi.Status.SandboxRuntime.ArtifactID).To(Equal("snap-pre"))
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})
})

// workerBindingNames lists the binding names cfmock recorded for a worker.
func workerBindingNames(url, script string) []string {
	st := fetchState(url)
	raw, ok := st.Result.Workers[script]
	if !ok {
		return nil
	}
	var ws struct {
		Bindings []map[string]any `json:"bindings"`
	}
	ExpectWithOffset(1, json.Unmarshal(raw, &ws)).To(Succeed())
	names := make([]string, 0, len(ws.Bindings))
	for _, b := range ws.Bindings {
		if n, ok := b["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

// workerBindingText returns the plain_text value of one binding.
func workerBindingText(url, script, binding string) string {
	st := fetchState(url)
	raw, ok := st.Result.Workers[script]
	if !ok {
		return fmt.Sprintf("<worker %s not found>", script)
	}
	var ws struct {
		Bindings []map[string]any `json:"bindings"`
	}
	ExpectWithOffset(1, json.Unmarshal(raw, &ws)).To(Succeed())
	for _, b := range ws.Bindings {
		if b["name"] == binding {
			if text, ok := b["text"].(string); ok {
				return text
			}
		}
	}
	return fmt.Sprintf("<binding %s not found>", binding)
}
