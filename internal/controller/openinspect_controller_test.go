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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
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
		sweepCalls int
		stopCalls  int
	)

	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	// cpSecretBindings are the control-plane worker secret bindings a valid
	// Modal spec must carry (the operator feeds them into the upstream
	// app's named Modal secrets).
	cpSecretBindings := func() []openinspectv1alpha1.WorkerSecret {
		ref := func(key string) openinspectv1alpha1.SecretKeyRef {
			return openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: key}
		}
		return []openinspectv1alpha1.WorkerSecret{
			{Binding: "GITHUB_CLIENT_SECRET", SecretRef: ref("githubClientSecret")},
			{Binding: "INTERNAL_CALLBACK_SECRET", SecretRef: ref("internalCallbackSecret")},
			{Binding: "GITHUB_APP_ID", SecretRef: ref("githubAppId")},
			{Binding: "GITHUB_APP_PRIVATE_KEY", SecretRef: ref("githubAppPrivateKey")},
			{Binding: "GITHUB_APP_INSTALLATION_ID", SecretRef: ref("githubAppInstallationId")},
		}
	}

	// modalSpec is a minimal valid spec with a Modal sandbox, parameterized on
	// the api-secret ref so tests can point it at a deletable secret.
	modalSpec := func(deploymentName string, apiRef openinspectv1alpha1.SecretKeyRef) openinspectv1alpha1.OpenInspectSpec {
		return openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: deploymentName,
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-123",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:    "control-plane",
				Script:  &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
				Secrets: cpSecretBindings(),
			}},
			AppSecrets: []openinspectv1alpha1.AppSecret{{
				Key:       "ANTHROPIC_API_KEY",
				SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "anthropicApiKey"},
			}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "modal",
				Modal: &openinspectv1alpha1.ModalSandboxSpec{
					Workspace:            "dirien",
					TokenIDSecretRef:     openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalTokenId"},
					TokenSecretSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalTokenSecret"},
					APISecretSecretRef:   apiRef,
				},
			},
		}
	}

	BeforeEach(func() {
		mock = httptest.NewServer(cfmock.New().Handler())
		DeferCleanup(mock.Close)
		store := &fakeSecretsAPI{secret: "ics-hunter2", store: map[string]string{}}
		storeServer := httptest.NewServer(store.handler(GinkgoT()))
		DeferCleanup(storeServer.Close)
		sweepCalls = 0
		stopCalls = 0
		reconciler = &OpenInspectReconciler{
			Client:   k8sClient,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(100),
			ModalCleanup: func(context.Context, *openinspectv1alpha1.ModalSandboxSpec, string, string) (int, bool, error) {
				sweepCalls++
				stopCalls++
				return 2, true, nil
			},
			// These specs cover the Cloudflare/platform surface; the
			// provider-app flow has its own Describe block.
			ProviderAppEnsure: func(context.Context, *openinspectv1alpha1.OpenInspect, *cfclient.Client) (bool, error) {
				return false, nil
			},
			AppSecretsURL: func(*openinspectv1alpha1.OpenInspect) string {
				return storeServer.URL + "/secrets"
			},
		}

		objs := []client.Object{
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cf-creds", Namespace: ns},
				StringData: map[string]string{"apiToken": "test-token"},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-secrets", Namespace: ns},
				StringData: map[string]string{
					"githubClientSecret":      "gh-hunter2",
					"modalTokenId":            "ak-test-id",
					"modalTokenSecret":        "as-test-secret",
					"modalApiSecret":          "modal-api-hunter2",
					"internalCallbackSecret":  "ics-hunter2",
					"githubAppId":             "12345",
					"githubAppPrivateKey":     "-----BEGIN RSA TEST KEY-----",
					"githubAppInstallationId": "67890",
					"anthropicApiKey":         "sk-ant-test",
					"daytonaApiKey":           "dtn-hunter2",
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
						Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
						Bindings: openinspectv1alpha1.WorkerBindings{
							D1:             []openinspectv1alpha1.D1Binding{{Binding: "DB"}},
							KV:             []openinspectv1alpha1.KVBinding{{Binding: "REPOS_CACHE", Namespace: "session-index"}},
							R2:             []openinspectv1alpha1.R2Binding{{Binding: "MEDIA_BUCKET", Bucket: "media"}},
							DurableObjects: []openinspectv1alpha1.DurableObjectBinding{{Binding: "SESSION", ClassName: "SessionDO"}},
							Services:       []openinspectv1alpha1.ServiceBinding{{Binding: "SLACK_BOT", Worker: "slack-bot"}},
						},
						Vars:    map[string]string{"APP_NAME": "Open Inspect"},
						Secrets: cpSecretBindings(),
						Cron:    []string{"* * * * *"},
					},
					{
						Name:   "slack-bot",
						Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "slack-bot.js"}},
						Bindings: openinspectv1alpha1.WorkerBindings{
							Services: []openinspectv1alpha1.ServiceBinding{{Binding: "CONTROL_PLANE", Worker: "control-plane"}},
						},
					},
				},
				AppSecrets: []openinspectv1alpha1.AppSecret{{
					Key:       "ANTHROPIC_API_KEY",
					SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "anthropicApiKey"},
				}},
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

		By("web UI deployed in-cluster with owner reference, hardened, and probed")
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/example/web:v1"))
		Expect(deploy.OwnerReferences).NotTo(BeEmpty())
		Expect(deploy.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
		webC := deploy.Spec.Template.Spec.Containers[0]
		Expect(webC.SecurityContext.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
		Expect(webC.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
		Expect(webC.ReadinessProbe.TCPSocket).NotTo(BeNil(), "probes must be TCP: arbitrary images may not serve HTTP 200 on /")
		Expect(webC.LivenessProbe.TCPSocket).NotTo(BeNil())
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
		Expect(sweepCalls).To(Equal(1), "deletion must sweep running Modal sandboxes")
		Expect(stopCalls).To(Equal(1), "deletion must stop the CR's Modal app deployment")
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
					Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
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

	It("maps referenced ConfigMaps and Secrets back to the CR", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "mapper",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				D1: &openinspectv1alpha1.D1Spec{MigrationsConfigMapRef: "d1-migrations"},
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

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "d1-migrations", Namespace: ns}}
		reqs := reconciler.mapReferencing(referencesConfigMap)(ctx, cm)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal(name))

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cf-creds", Namespace: ns}}
		Expect(reconciler.mapReferencing(referencesSecret)(ctx, secret)).To(HaveLen(1))

		unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: ns}}
		Expect(reconciler.mapReferencing(referencesConfigMap)(ctx, unrelated)).To(BeEmpty())
	})

	It("refuses a younger CR that claims the same deploymentName", func() {
		spec := openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "claim",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-123",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			KVNamespaces: []string{"one"},
		}
		a := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		b := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name + "-b", Namespace: ns}, Spec: spec}
		for _, oi := range []*openinspectv1alpha1.OpenInspect{a, b} {
			Expect(k8sClient.Create(ctx, oi)).To(Succeed())
			DeferCleanup(func() {
				latest := &openinspectv1alpha1.OpenInspect{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(oi), latest); err == nil {
					latest.Finalizers = nil
					_ = k8sClient.Update(ctx, latest)
					_ = k8sClient.Delete(ctx, latest)
				}
			})
		}

		Expect(reconcile()).To(Succeed(), "the older claimant must reconcile")
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
		Expect(err).To(HaveOccurred(), "the younger claimant must be refused")

		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), fetched)).To(Succeed())
		ready := meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("DeploymentNameConflict"))
		Expect(fetchState(mock.URL).Result.KV).To(HaveLen(1), "the refused CR must not touch remote state")

		Expect(reconcile()).To(Succeed(), "the older claimant must stay unaffected")
	})

	It("refuses pre-existing remote resources until adoptExisting is set", func() {
		spec := openinspectv1alpha1.OpenInspectSpec{
			DeploymentName: "takeover",
			DeletionPolicy: "Orphan",
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				AccountID:         "acc-123",
				APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
				APIBaseURL:        mock.URL,
			},
			D1:           &openinspectv1alpha1.D1Spec{},
			KVNamespaces: []string{"cache"},
			R2Buckets:    []string{"media"},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:   "control-plane",
				Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "control-plane.js"}},
			}},
		}

		By("first CR creates everything, then orphans it on delete")
		first := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, key, first)).To(Succeed())
		origID := first.Status.D1DatabaseID
		Expect(origID).NotTo(BeEmpty())
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
		Expect(fetchState(mock.URL).Result.D1).NotTo(BeEmpty(), "orphaned resources must survive")

		By("recreated CR conflicts instead of silently capturing the orphans")
		second := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		second.Spec.DeletionPolicy = ""
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		DeferCleanup(func() {
			latest := &openinspectv1alpha1.OpenInspect{}
			if err := k8sClient.Get(ctx, key, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
				_ = k8sClient.Delete(ctx, latest)
			}
		})
		Expect(reconcile()).To(HaveOccurred())
		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		ready := meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("ResourceConflict"))

		By("adoptExisting takes over the orphans instead of recreating them")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		fetched.Spec.AdoptExisting = true
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Status.D1DatabaseID).To(Equal(origID), "adoption must keep the original database")
		Expect(fetched.Status.Workers["control-plane"].AppliedHash).NotTo(BeEmpty())
		ready = meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		st := fetchState(mock.URL)
		Expect(st.Result.D1).To(HaveLen(1))
		Expect(st.Result.Workers).To(HaveLen(1))
	})

	It("blocks deletion on a non-empty bucket unless emptyBucketsOnDelete purges it", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "purge",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				R2Buckets: []string{"media"},
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
		Expect(fetchState(mock.URL).Result.R2).To(HaveKey("open-inspect-media-purge"))

		By("seeding an object into the bucket")
		req, err := http.NewRequest(http.MethodPut,
			mock.URL+"/accounts/acc-123/r2/buckets/open-inspect-media-purge/objects/sessions/img.png", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer x")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(200))
		_ = resp.Body.Close()

		By("deletion blocks while the bucket is non-empty")
		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).NotTo(Succeed(), "non-empty bucket must block deletion without the flag")
		Expect(fetchState(mock.URL).Result.R2).To(HaveKey("open-inspect-media-purge"))

		By("setting emptyBucketsOnDelete unblocks deletion")
		latest := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, latest)).To(Succeed())
		latest.Spec.EmptyBucketsOnDelete = true
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(fetchState(mock.URL).Result.R2).NotTo(HaveKey("open-inspect-media-purge"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
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

	It("suspends reconciliation and scales web to zero", func() {
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "suspend",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
				KVNamespaces: []string{"cache"},
				Web:          &openinspectv1alpha1.WebSpec{Image: "ghcr.io/example/web:v1"},
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
		Expect(testutil.ToFloat64(readyGauge.WithLabelValues(ns, name))).To(Equal(1.0))

		By("suspending scales web to zero and stops reconciling")
		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		fetched.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(BeEquivalentTo(0)))
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		ready := meta.FindStatusCondition(fetched.Status.Conditions, openinspectv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal("Suspended"))
		Expect(testutil.ToFloat64(readyGauge.WithLabelValues(ns, name))).To(Equal(0.0))

		By("spec edits cause no remote mutation while suspended")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		fetched.Spec.KVNamespaces = nil
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(fetchState(mock.URL).Result.KV).To(ContainElement("open-inspect-cache-suspend"),
			"suspension must block pruning")

		By("resuming restores web and applies the pending prune")
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		fetched.Spec.Suspended = false
		Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
		Expect(reconcile()).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-web", Namespace: ns}, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
		Expect(fetchState(mock.URL).Result.KV).To(BeEmpty())
		Expect(testutil.ToFloat64(readyGauge.WithLabelValues(ns, name))).To(Equal(1.0))
	})

	It("never touches an unrelated deployment with the web name", func() {
		// Own CR name: envtest has no GC, so the suspend test's owned
		// <name>-web deployment lingers and would collide.
		byName := name + "-by"
		byKey := types.NamespacedName{Name: byName, Namespace: ns}
		bystander := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: byName + "-web", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "bystander"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "bystander"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, bystander)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, bystander) })

		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: byName, Namespace: ns},
			Spec: openinspectv1alpha1.OpenInspectSpec{
				DeploymentName: "bystander",
				Cloudflare: openinspectv1alpha1.CloudflareSpec{
					AccountID:         "acc-123",
					APITokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "cf-creds", Key: "apiToken"},
					APIBaseURL:        mock.URL,
				},
			},
		}
		Expect(k8sClient.Create(ctx, oi)).To(Succeed())
		DeferCleanup(func() {
			latest := &openinspectv1alpha1.OpenInspect{}
			if err := k8sClient.Get(ctx, byKey, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
				_ = k8sClient.Delete(ctx, latest)
			}
		})
		reconcileBy := func() error {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: byKey})
			return err
		}

		By("a normal pass (spec.web nil) must not delete it")
		Expect(reconcileBy()).To(Succeed())
		fetched := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bystander), fetched)).To(Succeed())

		By("a suspended pass must not scale it")
		latest := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, byKey, latest)).To(Succeed())
		latest.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		Expect(reconcileBy()).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bystander), fetched)).To(Succeed())
		Expect(fetched.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)), "unowned deployment must keep its replicas")
	})

	It("leaves a shared Modal app running until its last CR is deleted", func() {
		apiRef := openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalApiSecret"}
		a := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: modalSpec("shared-a", apiRef)}
		b := &openinspectv1alpha1.OpenInspect{ObjectMeta: metav1.ObjectMeta{Name: name + "-b", Namespace: ns}, Spec: modalSpec("shared-b", apiRef)}
		for _, oi := range []*openinspectv1alpha1.OpenInspect{a, b} {
			Expect(k8sClient.Create(ctx, oi)).To(Succeed())
			DeferCleanup(func() {
				latest := &openinspectv1alpha1.OpenInspect{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(oi), latest); err == nil {
					latest.Finalizers = nil
					_ = k8sClient.Update(ctx, latest)
					_ = k8sClient.Delete(ctx, latest)
				}
			})
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oi)})
			Expect(err).NotTo(HaveOccurred())
		}

		By("deleting the first CR must not touch the shared app")
		Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
		Expect(sweepCalls).To(BeZero(), "shared sandbox pool must not be swept")
		Expect(stopCalls).To(BeZero(), "shared modal app must not be stopped")

		By("deleting the last CR stops the app")
		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
		Expect(err).NotTo(HaveOccurred())
		Expect(sweepCalls).To(Equal(1))
		Expect(stopCalls).To(Equal(1))
	})

	It("blocks deletion while the Modal token secret is missing", func() {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cp-secrets", Namespace: ns}}
		oi := &openinspectv1alpha1.OpenInspect{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       modalSpec("modal-creds", openinspectv1alpha1.SecretKeyRef{Name: "cp-secrets", Key: "modalApiSecret"}),
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

		By("deletion blocks while the secret is gone")
		Expect(k8sClient.Delete(ctx, sec)).To(Succeed())
		Expect(k8sClient.Delete(ctx, oi)).To(Succeed())
		Expect(reconcile()).NotTo(Succeed(), "missing modal credentials must block deletion")
		fetched := &openinspectv1alpha1.OpenInspect{}
		Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
		Expect(fetched.Finalizers).To(ContainElement(finalizer))
		Expect(stopCalls).To(BeZero(), "the app must not be stopped without credentials")

		By("restoring the secret unblocks deletion")
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-secrets", Namespace: ns},
			StringData: map[string]string{
				"githubClientSecret": "gh-hunter2",
				"modalTokenId":       "ak-test-id",
				"modalTokenSecret":   "as-test-secret",
				"modalApiSecret":     "modal-api-hunter2",
			},
		}
		Expect(k8sClient.Create(ctx, sec)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sec) })
		Expect(reconcile()).To(Succeed())
		Expect(stopCalls).To(Equal(1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &openinspectv1alpha1.OpenInspect{}))).To(BeTrue())
	})
})
