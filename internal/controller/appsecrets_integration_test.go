package controller

import (
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
)

var _ = Describe("AppSecrets seeding", func() {
	const name = "appsecrets-cr"
	ns := "default"
	key := types.NamespacedName{Name: name, Namespace: ns}
	var (
		mock        *httptest.Server
		store       *fakeSecretsAPI
		storeServer *httptest.Server
		reconciler  *OpenInspectReconciler
	)

	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	fetch := func() *openinspectv1alpha1.OpenInspect {
		oi := &openinspectv1alpha1.OpenInspect{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, oi)).To(Succeed())
		return oi
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		store = &fakeSecretsAPI{secret: "ics-hunter2", store: map[string]string{"FOREIGN": "keep"}}
		store.puts = 0
		storeServer = httptest.NewServer(store.handler(GinkgoT()))
		DeferCleanup(storeServer.Close)
		reconciler = &OpenInspectReconciler{
			Client:   k8sClient,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(100),
			AppSecretsURL: func(*openinspectv1alpha1.OpenInspect) string {
				return storeServer.URL + "/secrets"
			},
		}

		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "as-cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "as-platform", Namespace: ns},
				StringData: map[string]string{
					"ics":       "ics-hunter2",
					"anthropic": "sk-ant-test",
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "as-bundles", Namespace: ns},
				Data:       map[string]string{"cp.js": "export default { fetch() { return new Response('cp') } }"},
			},
		}
		for _, obj := range objs {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
	})

	It("seeds, skips unchanged, prunes removed keys, and survives teardown", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "asx",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-appsecrets",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "as-cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				Workers: []openinspectv1alpha1.WorkerSpec{{
					Name:   "control-plane",
					Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "as-bundles", Key: "cp.js"}},
					Secrets: []openinspectv1alpha1.WorkerSecret{{
						Binding:   internalCallbackSecretBinding,
						SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "as-platform", Key: "ics"},
					}},
				}},
				AppSecrets: []openinspectv1alpha1.AppSecret{{
					Key:       "ANTHROPIC_API_KEY",
					SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "as-platform", Key: "anthropic"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		DeferCleanup(func() {
			obj := &openinspectv1alpha1.OpenInspect{}
			if err := k8sClient.Get(ctx, key, obj); err == nil {
				_ = k8sClient.Delete(ctx, obj)
				_ = reconcile()
			}
		})

		By("seeding the store on first reconcile")
		Expect(reconcile()).To(Succeed())
		Expect(store.store).To(HaveKeyWithValue("ANTHROPIC_API_KEY", "sk-ant-test"))
		Expect(store.store).To(HaveKeyWithValue("FOREIGN", "keep"))
		got := fetch()
		Expect(got.Status.AppSecrets).NotTo(BeNil())
		Expect(got.Status.AppSecrets.Keys).To(Equal([]string{"ANTHROPIC_API_KEY"}))
		cond := meta.FindStatusCondition(got.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		By("skipping the PUT when nothing changed")
		puts := store.puts
		Expect(reconcile()).To(Succeed())
		Expect(store.puts).To(Equal(puts))

		By("re-seeding when the store is wiped out of band")
		delete(store.store, "ANTHROPIC_API_KEY")
		Expect(reconcile()).To(Succeed())
		Expect(store.store).To(HaveKeyWithValue("ANTHROPIC_API_KEY", "sk-ant-test"))

		By("re-seeding when the backing Secret value rotates")
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "as-platform", Namespace: ns}, sec)).To(Succeed())
		sec.Data["anthropic"] = []byte("sk-ant-rotated")
		Expect(k8sClient.Update(ctx, sec)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(store.store).To(HaveKeyWithValue("ANTHROPIC_API_KEY", "sk-ant-rotated"))

		By("pruning a key removed from spec while leaving foreign keys alone")
		cur := fetch()
		cur.Spec.AppSecrets = nil
		Expect(k8sClient.Update(ctx, cur)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(store.store).NotTo(HaveKey("ANTHROPIC_API_KEY"))
		Expect(store.store).To(HaveKeyWithValue("FOREIGN", "keep"))
		got = fetch()
		Expect(got.Status.AppSecrets).To(BeNil())
		Expect(meta.FindStatusCondition(got.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)).To(BeNil())

		By("not wedging when appSecrets and the ICS binding are removed together")
		cur = fetch()
		cur.Spec.AppSecrets = []openinspectv1alpha1.AppSecret{{
			Key:       "ANTHROPIC_API_KEY",
			SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "as-platform", Key: "anthropic"},
		}}
		Expect(k8sClient.Update(ctx, cur)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(store.store).To(HaveKey("ANTHROPIC_API_KEY"))
		cur = fetch()
		cur.Spec.AppSecrets = nil
		cur.Spec.Workers[0].Secrets = nil
		Expect(k8sClient.Update(ctx, cur)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		got = fetch()
		Expect(got.Status.AppSecrets).To(BeNil())
		Expect(meta.FindStatusCondition(got.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)).To(BeNil())
		// The orphaned key stays in the store — the operator cannot reach it
		// without the ICS binding and must not wedge on it.
		Expect(store.store).To(HaveKey("ANTHROPIC_API_KEY"))
	})
})
