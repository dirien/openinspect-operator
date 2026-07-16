/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

type mockState struct {
	Result struct {
		D1           map[string]string          `json:"d1"`
		D1Migrations map[string][]string        `json:"d1_migrations"`
		KV           map[string]string          `json:"kv"`
		R2           map[string]bool            `json:"r2"`
		Workers      map[string]json.RawMessage `json:"workers"`
	} `json:"result"`
}

func fetchState(url string) mockState {
	resp, err := http.Get(url + "/__state")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	var st mockState
	Expect(json.NewDecoder(resp.Body).Decode(&st)).To(Succeed())
	return st
}

var _ = Describe("OpenInspect Controller", func() {
	const name = "test-deploy"
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

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		reconciler = &OpenInspectReconciler{
			Client:   k8sClient,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(100),
		}

		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-secrets", Namespace: ns},
				StringData: map[string]string{
					"githubClientSecret": "gh-hunter2",
					"modalTokenId":       "ak-test-id",
					"modalTokenSecret":   "as-test-secret",
					"modalApiSecret":     "modal-api-hunter2",
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "d1-migrations", Namespace: ns},
				Data: map[string]string{
					"0001_init.sql":  "CREATE TABLE sessions (id TEXT PRIMARY KEY);",
					"0002_users.sql": "CREATE TABLE users (id TEXT PRIMARY KEY);\nCREATE INDEX idx_users ON users(id);",
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "bundles", Namespace: ns},
				Data: map[string]string{
					"control-plane.js": "export default { fetch() { return new Response('cp') } }",
					"slack-bot.js":     "export default { fetch() { return new Response('slack') } }",
				},
			},
		}
		for _, obj := range objs {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}
	})

	It("reconciles the full platform and cleans up on delete", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "prod",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				D1:           &openinspectv1alpha1.D1Spec{MigrationsConfigMapRef: "d1-migrations"},
				KVNamespaces: []string{"session-index"},
				R2Buckets:    []string{"media"},
				Workers: []openinspectv1alpha1.WorkerSpec{
					{
						Name:   "control-plane",
						Script: openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
						Bindings: openinspectv1alpha1.WorkerBindings{
							D1:             []openinspectv1alpha1.D1Binding{{Binding: "DB"}},
							KV:             []openinspectv1alpha1.KVBinding{{Binding: "REPOS_CACHE", Namespace: "session-index"}},
							R2:             []openinspectv1alpha1.R2Binding{{Binding: "MEDIA_BUCKET", Bucket: "media"}},
							DurableObjects: []openinspectv1alpha1.DurableObjectBinding{{Binding: "SESSION", ClassName: "SessionDO"}},
							Services:       []openinspectv1alpha1.ServiceBinding{{Binding: "SLACK_BOT", Worker: "slack-bot"}},
						},
						Vars:    map[string]string{"APP_NAME": "Open Inspect"},
						Secrets: []openinspectv1alpha1.WorkerSecret{{Binding: "GITHUB_CLIENT_SECRET", SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "githubClientSecret"}}},
						Cron:    []string{"* * * * *"},
					},
					{
						Name:   "slack-bot",
						Script: openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "slack-bot.js"}},
						Bindings: openinspectv1alpha1.WorkerBindings{
							Services: []openinspectv1alpha1.ServiceBinding{{Binding: "CONTROL_PLANE", Worker: "control-plane"}},
						},
					},
				},
				Sandbox: &openinspectv1alpha1.SandboxSpec{
					Provider: "modal",
					Modal: &openinspectv1alpha1.ModalSandboxSpec{
						Workspace:            "dirien",
						TokenIDSecretRef:     openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalTokenId"},
						TokenSecretSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalTokenSecret"},
						APISecretSecretRef:   openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalApiSecret"},
					},
				},
				Web: &openinspectv1alpha1.WebSpec{
					Image: "ghcr.io/example/web:v1",
					Env:   map[string]string{"CONTROL_PLANE_URL": "https://cp.example.com"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		DeferCleanup(func() {
			latest := &openinspectv1alpha1.OpenInspect{}
			if err := k8sClient.Get(ctx, key, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
				_ = k8sClient.Delete(ctx, latest)
			}
		})

		By("first pass: everything created, circular service binding pending")
		Expect(reconcile()).To(Succeed())
		st := fetchState(mock.URL)
		Expect(st.Result.D1).To(HaveKey("open-inspect-db-prod"))
		Expect(st.Result.D1Migrations["open-inspect-db-prod"]).To(Equal([]string{"0001_init.sql", "0002_users.sql"}))
		Expect(st.Result.KV).To(ContainElement("open-inspect-session-index-prod"))
		Expect(st.Result.R2).To(HaveKey("open-inspect-media-prod"))
		Expect(st.Result.Workers).To(HaveKey("open-inspect-control-plane-prod"))
		Expect(st.Result.Workers).To(HaveKey("open-inspect-slack-bot-prod"))

		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Finalizers).To(ContainElement(finalizer))
		Expect(fetched.Status.AppliedMigrations).To(Equal(2))
		ready := meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("ServiceBindingsPending"))

		By("second pass: circular binding converges, Ready=True")
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		ready = meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		st = fetchState(mock.URL)
		cp := string(st.Result.Workers["open-inspect-control-plane-prod"])
		Expect(cp).To(ContainSubstring(`"service":"open-inspect-slack-bot-prod"`))
		Expect(cp).To(ContainSubstring(`"text":"gh-hunter2"`))
		Expect(cp).To(ContainSubstring(`"SessionDO"`))
		By("sandbox provider config injected into the control-plane worker")
		Expect(cp).To(ContainSubstring(`"name":"SANDBOX_PROVIDER","text":"modal"`))
		Expect(cp).To(ContainSubstring(`"name":"MODAL_WORKSPACE","text":"dirien"`))
		Expect(cp).To(ContainSubstring(`"name":"MODAL_TOKEN_ID","text":"ak-test-id"`))
		Expect(cp).To(ContainSubstring(`"name":"MODAL_ENVIRONMENT","text":"main"`), "environment default must apply")
		Expect(cp).To(ContainSubstring(`"* * * * *"`))
		Expect(cp).To(ContainSubstring(`"subdomain":true`))

		By("web UI deployed in-cluster with owner reference")
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/example/web:v1"))
		Expect(deploy.OwnerReferences).NotTo(BeEmpty())
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, svc)).To(Succeed())

		By("third pass: no version churn when nothing changed")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		hashBefore := fetched.Status.Workers["control-plane"].AppliedHash
		Expect(hashBefore).NotTo(BeEmpty())
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.Workers["control-plane"].AppliedHash).To(Equal(hashBefore))

		By("new migration applied incrementally")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "d1-migrations", Namespace: ns}, cm)).To(Succeed())
		cm.Data["0003_extra.sql"] = "CREATE TABLE extra (id TEXT);"
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.AppliedMigrations).To(Equal(3))

		By("pruning: removing slack-bot, its binding, cron, and web from spec")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		fetched.Spec.Workers = fetched.Spec.Workers[:1]
		fetched.Spec.Workers[0].Bindings.Services = nil
		fetched.Spec.Workers[0].Cron = nil
		fetched.Spec.Web = nil
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		st = fetchState(mock.URL)
		Expect(st.Result.Workers).NotTo(HaveKey("open-inspect-slack-bot-prod"), "removed worker must be deleted remotely")
		Expect(string(st.Result.Workers["open-inspect-control-plane-prod"])).To(ContainSubstring(`"cron":[]`),
			"emptied cron must clear remote schedules")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, &appsv1.Deployment{}))
		}).Should(BeTrue(), "web deployment must be removed when spec.web is nil")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.Workers).NotTo(HaveKey("slack-bot"))
		Expect(meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionWeb)).To(BeNil())

		By("deletion cleans up all Cloudflare resources")
		Expect(k8sClient.Delete(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		st = fetchState(mock.URL)
		Expect(st.Result.D1).To(BeEmpty())
		Expect(st.Result.KV).To(BeEmpty())
		Expect(st.Result.R2).To(BeEmpty())
		Expect(st.Result.Workers).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
	})

	It("rejects specs with dangling references as InvalidSpec", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "bad",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				Workers: []openinspectv1alpha1.WorkerSpec{{
					Name:   "control-plane",
					Script: openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
					Bindings: openinspectv1alpha1.WorkerBindings{
						KV: []openinspectv1alpha1.KVBinding{{Binding: "X", Namespace: "does-not-exist"}},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, oi) })
		Expect(reconcile()).To(Succeed())
		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		ready := meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("InvalidSpec"))
		Expect(fetchState(mock.URL).Result.Workers).To(BeEmpty(), "nothing must be created for an invalid spec")
	})

	It("prunes storage removed from spec", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "prune",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				KVNamespaces: []string{"one", "two"},
				R2Buckets:    []string{"data"},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		DeferCleanup(func() {
			latest := &openinspectv1alpha1.OpenInspect{}
			if err := k8sClient.Get(ctx, key, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
				_ = k8sClient.Delete(ctx, latest)
			}
		})
		Expect(reconcile()).To(Succeed())
		Expect(fetchState(mock.URL).Result.KV).To(HaveLen(2))

		Expect(k8sClient.Get(ctx, key, oi)).To(Succeed())
		oi.Spec.KVNamespaces = []string{"one"}
		oi.Spec.R2Buckets = nil
		Expect(k8sClient.Update(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		st := fetchState(mock.URL)
		Expect(st.Result.KV).To(HaveLen(1))
		Expect(st.Result.KV).To(ContainElement("open-inspect-one-prune"))
		Expect(st.Result.R2).To(BeEmpty())
		Expect(k8sClient.Get(ctx, key, oi)).To(Succeed())
		Expect(oi.Status.KVNamespaceIDs).To(HaveLen(1))
		Expect(oi.Status.R2Buckets).To(BeEmpty())
	})

	It("orphans Cloudflare resources with deletionPolicy Orphan", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "orphan",
				DeletionPolicy: "Orphan",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				KVNamespaces: []string{"cache"},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(fetchState(mock.URL).Result.KV).To(ContainElement("open-inspect-cache-orphan"))

		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(fetchState(mock.URL).Result.KV).To(ContainElement("open-inspect-cache-orphan"),
			"orphan policy must keep remote resources")
	})
})
