package controller

import (
	"testing"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

func ref(key string) openinspectv1alpha1.SecretKeyRef {
	return openinspectv1alpha1.SecretKeyRef{Name: "s", Key: key}
}

func TestSandboxInjection(t *testing.T) {
	cases := []struct {
		name        string
		sb          openinspectv1alpha1.SandboxSpec
		wantVars    map[string]string
		wantSecrets []string
	}{
		{
			name: "modal",
			sb: openinspectv1alpha1.SandboxSpec{
				Provider: "modal",
				Modal: &openinspectv1alpha1.ModalSandboxSpec{
					Workspace: "dirien", Environment: "main",
					TokenIDSecretRef: ref("id"), TokenSecretSecretRef: ref("tok"), APISecretSecretRef: ref("api"),
				},
			},
			wantVars:    map[string]string{"SANDBOX_PROVIDER": "modal", "MODAL_WORKSPACE": "dirien", "MODAL_ENVIRONMENT": "main"},
			wantSecrets: []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "MODAL_API_SECRET"},
		},
		{
			name: "daytona",
			sb: openinspectv1alpha1.SandboxSpec{
				Provider:            "daytona",
				InactivityTimeoutMS: 600000,
				Daytona: &openinspectv1alpha1.DaytonaSandboxSpec{
					BaseSnapshot: "snap-1", APIURL: "https://app.daytona.io/api", Target: "eu",
					APIKeySecretRef: ref("key"),
				},
			},
			wantVars: map[string]string{
				"SANDBOX_PROVIDER": "daytona", "DAYTONA_BASE_SNAPSHOT": "snap-1",
				"DAYTONA_API_URL": "https://app.daytona.io/api", "DAYTONA_TARGET": "eu",
				"SANDBOX_INACTIVITY_TIMEOUT_MS": "600000",
			},
			wantSecrets: []string{"DAYTONA_API_KEY"},
		},
		{
			name: "vercel",
			sb: openinspectv1alpha1.SandboxSpec{
				Provider: "vercel",
				Vercel: &openinspectv1alpha1.VercelSandboxSpec{
					ProjectID: "prj_1", Runtime: "node24", TeamID: "team_1",
					BaseSnapshotID: "snap_2", SnapshotExpirationMS: 3600000,
					TokenSecretRef: ref("tok"),
				},
			},
			wantVars: map[string]string{
				"SANDBOX_PROVIDER": "vercel", "VERCEL_PROJECT_ID": "prj_1", "VERCEL_RUNTIME": "node24",
				"VERCEL_TEAM_ID": "team_1", "VERCEL_BASE_SNAPSHOT_ID": "snap_2",
				"VERCEL_SNAPSHOT_EXPIRATION_MS": "3600000",
			},
			wantSecrets: []string{"VERCEL_TOKEN"},
		},
		{
			name: "opencomputer",
			sb: openinspectv1alpha1.SandboxSpec{
				Provider: "opencomputer",
				OpenComputer: &openinspectv1alpha1.OpenComputerSandboxSpec{
					APIURL: "https://api.opencomputer.dev", Template: "tmpl-1",
					APIKeySecretRef: ref("key"),
				},
			},
			wantVars: map[string]string{
				"SANDBOX_PROVIDER": "opencomputer", "OPENCOMPUTER_API_URL": "https://api.opencomputer.dev",
				"OPENCOMPUTER_TEMPLATE": "tmpl-1",
			},
			wantSecrets: []string{"OPENCOMPUTER_API_KEY"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars, secrets := sandboxInjection(&tc.sb)
			for k, v := range tc.wantVars {
				if vars[k] != v {
					t.Errorf("var %s = %q, want %q", k, vars[k], v)
				}
			}
			got := map[string]bool{}
			for _, s := range secrets {
				got[s.Binding] = true
			}
			for _, b := range tc.wantSecrets {
				if !got[b] {
					t.Errorf("missing secret binding %s", b)
				}
			}
			if len(secrets) != len(tc.wantSecrets) {
				t.Errorf("got %d secrets, want %d", len(secrets), len(tc.wantSecrets))
			}
		})
	}
}

func TestApplySandboxUserValuesWin(t *testing.T) {
	oi := &openinspectv1alpha1.OpenInspect{
		Spec: openinspectv1alpha1.OpenInspectSpec{
			Sandbox: &openinspectv1alpha1.SandboxSpec{
				Provider: "modal",
				Modal: &openinspectv1alpha1.ModalSandboxSpec{
					Workspace:        "dirien",
					TokenIDSecretRef: ref("id"), TokenSecretSecretRef: ref("tok"), APISecretSecretRef: ref("api"),
				},
			},
		},
	}
	w := openinspectv1alpha1.WorkerSpec{
		Name: "control-plane",
		Vars: map[string]string{"MODAL_WORKSPACE": "override"},
		Secrets: []openinspectv1alpha1.WorkerSecret{
			{Binding: "MODAL_TOKEN_ID", SecretRef: ref("user-id")},
		},
	}
	got := applySandbox(oi, w)
	if got.Vars["MODAL_WORKSPACE"] != "override" {
		t.Errorf("explicit var must win, got %q", got.Vars["MODAL_WORKSPACE"])
	}
	if got.Vars["SANDBOX_PROVIDER"] != "modal" {
		t.Errorf("injected var missing")
	}
	count := 0
	for _, s := range got.Secrets {
		if s.Binding == "MODAL_TOKEN_ID" {
			count++
			if s.SecretRef.Key != "user-id" {
				t.Errorf("explicit secret must win, got key %q", s.SecretRef.Key)
			}
		}
	}
	if count != 1 {
		t.Errorf("MODAL_TOKEN_ID appears %d times, want 1", count)
	}
	// untargeted worker untouched
	other := applySandbox(oi, openinspectv1alpha1.WorkerSpec{Name: "github-bot"})
	if len(other.Vars) != 0 {
		t.Errorf("non-target worker must not receive injection")
	}
}

func TestValidateSandbox(t *testing.T) {
	workers := map[string]bool{"control-plane": true}
	if err := validateSandbox(&openinspectv1alpha1.SandboxSpec{Provider: "daytona"}, workers); err == nil {
		t.Error("provider without matching section must fail")
	}
	if err := validateSandbox(&openinspectv1alpha1.SandboxSpec{
		Provider: "modal",
		Modal:    &openinspectv1alpha1.ModalSandboxSpec{Workspace: "w"},
		Daytona:  &openinspectv1alpha1.DaytonaSandboxSpec{BaseSnapshot: "s"},
	}, workers); err == nil {
		t.Error("extra provider section must fail")
	}
	if err := validateSandbox(&openinspectv1alpha1.SandboxSpec{
		Provider:           "modal",
		ControlPlaneWorker: "missing",
		Modal:              &openinspectv1alpha1.ModalSandboxSpec{Workspace: "w"},
	}, workers); err == nil {
		t.Error("unknown controlPlaneWorker must fail")
	}
	if err := validateSandbox(&openinspectv1alpha1.SandboxSpec{
		Provider: "modal",
		Modal:    &openinspectv1alpha1.ModalSandboxSpec{Workspace: "w"},
	}, workers); err != nil {
		t.Errorf("valid spec rejected: %v", err)
	}
}
