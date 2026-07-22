package controller

import (
	"testing"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

func TestReferenceMatchers(t *testing.T) {
	oi := &openinspectv1alpha1.OpenInspect{
		Spec: openinspectv1alpha1.OpenInspectSpec{
			Cloudflare: openinspectv1alpha1.CloudflareSpec{
				APITokenSecretRef: ref("apiToken"),
			},
			D1: &openinspectv1alpha1.D1Spec{MigrationsConfigMapRef: "migrations"},
			Workers: []openinspectv1alpha1.WorkerSpec{{
				Name:   "control-plane",
				Script: &openinspectv1alpha1.ScriptSource{ConfigMapRef: &openinspectv1alpha1.ConfigMapKeyRef{Name: "bundles", Key: "cp.js"}},
				Secrets: []openinspectv1alpha1.WorkerSecret{
					{Binding: "X", SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "worker-secrets", Key: "x"}},
				},
			}},
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "vercel",
				Vercel: &openinspectv1alpha1.VercelSandboxSpec{
					ProjectID:      "p",
					TokenSecretRef: openinspectv1alpha1.SecretKeyRef{Name: "vercel-secrets", Key: "t"},
				},
			},
			Web: &openinspectv1alpha1.WebSpec{
				Image: "img",
				SecretEnv: []openinspectv1alpha1.WorkerSecret{
					{Binding: "Y", SecretRef: openinspectv1alpha1.SecretKeyRef{Name: "web-secrets", Key: "y"}},
				},
			},
		},
	}
	for _, name := range []string{"migrations", "bundles"} {
		if !referencesConfigMap(oi, name) {
			t.Errorf("configmap %q should match", name)
		}
	}
	if referencesConfigMap(oi, "unrelated") {
		t.Error("unrelated configmap must not match")
	}
	// "s" is the secret name used by ref() for the cloudflare token.
	for _, name := range []string{"s", "worker-secrets", "vercel-secrets", "web-secrets"} {
		if !referencesSecret(oi, name) {
			t.Errorf("secret %q should match", name)
		}
	}
	if referencesSecret(oi, "unrelated") {
		t.Error("unrelated secret must not match")
	}
}
