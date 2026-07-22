package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

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
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

// Artifact garbage collection and finalization (EPIC1 D6 + section 8): the
// grace-period GC of superseded artifacts, deletion of the managed base
// artifact and recorded prebuild images on CR deletion, and the Orphan
// escape hatch.
var _ = Describe("Managed artifact cleanup", func() {
	const name = "artifact-cleanup"
	ns := "default"
	key := types.NamespacedName{Name: name, Namespace: ns}
	var (
		mock          *httptest.Server
		reconciler    *OpenInspectReconciler
		deletedNames  []string
		deletedImages []cfclient.ImageRef
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

	spec := func(runtimeDeletion string) openinspectv1alpha1.OpenInspectSpec {
		s := openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "gc",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-gc",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "gc-cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			// Embedded migrations create the image_builds evidence table.
			D1: &openinspectv1alpha1.D1Spec{},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:     "control-plane",
				Bindings: openinspectv1alpha1.WorkerBindings{D1: []openinspectv1alpha1.D1Binding{{Binding: "DB"}}},
			}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "daytona",
				Daytona: &openinspectv1alpha1.DaytonaSandboxSpec{
					APIURL:          "https://daytona.invalid/api",
					APIKeySecretRef: openinspectv1alpha1.SecretKeyRef{Name: "gc-secrets", Key: "daytonaApiKey"},
				},
			},
		}
		if runtimeDeletion != "" {
			s.Sandbox.Runtime = &openinspectv1alpha1.SandboxRuntimeSpec{DeletionPolicy: runtimeDeletion}
		}
		return s
	}

	// converge drives the CR to a Ready managed artifact (fake provider).
	converge := func() {
		ExpectWithOffset(1, reconcile()).To(Succeed())
		oi := getOI()
		ExpectWithOffset(1, oi.Status.SandboxRuntime).NotTo(BeNil())
		ExpectWithOffset(1, oi.Status.SandboxRuntime.Phase).To(Equal(openinspectv1alpha1.RuntimePhaseReady))
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		deletedNames = nil
		deletedImages = nil
		reconciler = &OpenInspectReconciler{
			Client:              k8sClient,
			Scheme:              scheme.Scheme,
			Recorder:            record.NewFakeRecorder(200),
			RuntimeBuilderImage: "example.com/openinspect-operator:test",
			// The artifact exists remotely from the start: converge without
			// a build Job.
			RuntimeInspect: func(context.Context, *openinspectv1alpha1.OpenInspect, provider.Artifact) (provider.ArtifactState, error) {
				return provider.ArtifactState{Phase: provider.ArtifactReady, ID: "snap-live"}, nil
			},
			RuntimeDelete: func(_ context.Context, _ *openinspectv1alpha1.OpenInspect, artifact provider.Artifact) error {
				deletedNames = append(deletedNames, artifact.Name)
				return nil
			},
			ProviderImageDelete: func(_ context.Context, _ *openinspectv1alpha1.OpenInspect, ref cfclient.ImageRef) error {
				deletedImages = append(deletedImages, ref)
				return nil
			},
			DaytonaCleanup: func(context.Context, *openinspectv1alpha1.DaytonaSandboxSpec, string, map[string]struct{}) (int, error) {
				return 0, nil
			},
		}
		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "gc-cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "gc-secrets", Namespace: ns},
				StringData: map[string]string{"daytonaApiKey": "dtn-hunter2"},
			},
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

	retire := func(name string, age time.Duration) {
		oi := getOI()
		oi.Status.SandboxRuntime.PreviousArtifacts = append(oi.Status.SandboxRuntime.PreviousArtifacts,
			openinspectv1alpha1.RetiredArtifact{Name: name, RetiredAt: metav1.NewTime(time.Now().Add(-age))})
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, oi)).To(Succeed())
	}

	It("garbage-collects retired artifacts only after the grace period", func() {
		Expect(k8sClient.Create(ctx, &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec(""),
		})).To(Succeed())
		converge()
		retire("oi-old-expired", 25*time.Hour)
		retire("oi-old-fresh", time.Hour)

		Expect(reconcile()).To(Succeed())
		Expect(deletedNames).To(Equal([]string{"oi-old-expired"}))
		st := getOI().Status.SandboxRuntime
		Expect(st.PreviousArtifacts).To(HaveLen(1))
		Expect(st.PreviousArtifacts[0].Name).To(Equal("oi-old-fresh"))
	})

	It("retains retired artifacts under runtime.deletionPolicy Orphan", func() {
		Expect(k8sClient.Create(ctx, &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec("Orphan"),
		})).To(Succeed())
		converge()
		retire("oi-old-expired", 25*time.Hour)

		Expect(reconcile()).To(Succeed())
		Expect(deletedNames).To(BeEmpty())
		Expect(getOI().Status.SandboxRuntime.PreviousArtifacts).To(HaveLen(1))
	})

	It("deletes the managed artifact, retired artifacts, and recorded images on CR deletion", func() {
		Expect(k8sClient.Create(ctx, &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec(""),
		})).To(Succeed())
		converge()
		retire("oi-old-fresh", time.Hour)
		oi := getOI()
		active := oi.Status.SandboxRuntime.ArtifactName

		// The platform recorded a prebuild image in image_builds (created by
		// the embedded upstream migrations).
		body := `{"sql":"INSERT INTO image_builds (id, scope_kind, scope_id, provider, provider_image_id, repositories_fingerprint, repository_shas, runtime_version, status, created_at) VALUES ('ib-1','repo','o/r','daytona','snap-repo-1','fp','[]','v1','ready', 1)"}`
		req, err := http.NewRequest(http.MethodPost,
			mock.URL+"/accounts/acc-gc/d1/database/"+oi.Status.D1DatabaseID+"/query", strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())

		Expect(deletedImages).To(Equal([]cfclient.ImageRef{{Provider: "daytona", ImageID: "snap-repo-1"}}))
		Expect(deletedNames).To(ContainElements(active, "oi-old-fresh"))
	})

	It("orphans everything under runtime.deletionPolicy Orphan on CR deletion", func() {
		Expect(k8sClient.Create(ctx, &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec("Orphan"),
		})).To(Succeed())
		converge()

		recorder := record.NewFakeRecorder(200)
		reconciler.Recorder = recorder
		oi := getOI()
		active := oi.Status.SandboxRuntime.ArtifactName
		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
		Expect(deletedNames).To(BeEmpty())

		var orphanEvent string
		for len(recorder.Events) > 0 {
			e := <-recorder.Events
			if strings.Contains(e, "RuntimeArtifactsOrphaned") {
				orphanEvent = e
			}
		}
		Expect(orphanEvent).To(ContainSubstring(active),
			"the orphan event must carry the complete retained references")
	})

	It("fails deletion closed when artifact deletion fails", func() {
		Expect(k8sClient.Create(ctx, &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec(""),
		})).To(Succeed())
		converge()
		reconciler.RuntimeDelete = func(context.Context, *openinspectv1alpha1.OpenInspect, provider.Artifact) error {
			return fmt.Errorf("provider timeout")
		}
		oi := getOI()
		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(HaveOccurred(), "unknown provider state must retain the finalizer")
		Expect(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{})).To(Succeed(),
			"the CR must still exist while cleanup is unconfirmed")
	})
})
