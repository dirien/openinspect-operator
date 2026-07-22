package daytona

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

func newTestProvider(t *testing.T, url string) *Provider {
	t.Helper()
	p, err := New(Config{APIKey: "test-key", APIURL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func snapshotPayload(name, state string, errorReason string) map[string]any {
	payload := map[string]any{
		"id":             "id-" + name,
		"organizationId": "org-1",
		"general":        false,
		"name":           name,
		"imageName":      name + ":img",
		"state":          state,
		"size":           1,
		"entrypoint":     []string{"python", "-m", "sandbox_runtime.entrypoint"},
		"cpu":            1, "gpu": 0, "mem": 2, "disk": 10,
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z",
		// Required nullable properties of the DTO.
		"errorReason": nil,
		"lastUsedAt":  nil,
	}
	if errorReason != "" {
		payload["errorReason"] = errorReason
	}
	return payload
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{APIURL: "https://x"}); err == nil {
		t.Error("empty api key must be rejected")
	}
	// An empty URL would silently fall back to the SDK's public default and
	// operate on the wrong Daytona deployment.
	if _, err := New(Config{APIKey: "k"}); err == nil {
		t.Error("empty api url must be rejected")
	}
}

func TestInspectBaseArtifactStateMapping(t *testing.T) {
	cases := []struct {
		state   string
		reason  string
		want    provider.ArtifactPhase
		wantMsg string
	}{
		{state: "active", want: provider.ArtifactReady},
		{state: "inactive", want: provider.ArtifactReady},
		{state: "building", want: provider.ArtifactBuilding},
		{state: "pending", want: provider.ArtifactBuilding},
		{state: "pulling", want: provider.ArtifactBuilding},
		{state: "removing", want: provider.ArtifactBuilding},
		{state: "error", reason: "boom", want: provider.ArtifactFailed, wantMsg: "boom"},
		{state: "build_failed", reason: "no space", want: provider.ArtifactFailed, wantMsg: "no space"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/snapshots/base" && r.Method == http.MethodGet {
					writeJSON(t, w, http.StatusOK, snapshotPayload("base", tc.state, tc.reason))
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			state, err := newTestProvider(t, server.URL).InspectBaseArtifact(
				context.Background(), provider.Artifact{Name: "base"})
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if state.Phase != tc.want {
				t.Errorf("state %q mapped to %q, want %q", tc.state, state.Phase, tc.want)
			}
			if state.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", state.Message, tc.wantMsg)
			}
			if state.ID != "id-base" {
				t.Errorf("id = %q, want id-base", state.ID)
			}
		})
	}
}

func TestInspectBaseArtifactAbsent(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	state, err := newTestProvider(t, server.URL).InspectBaseArtifact(
		context.Background(), provider.Artifact{Name: "missing"})
	if err != nil {
		t.Fatalf("inspect absent: %v", err)
	}
	if state.Phase != provider.ArtifactAbsent {
		t.Errorf("phase = %q, want Absent", state.Phase)
	}
}

func TestInspectBaseArtifactServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	// A 5xx is unknown state, never "absent": callers must retry, not build
	// over a possibly existing artifact.
	if _, err := newTestProvider(t, server.URL).InspectBaseArtifact(
		context.Background(), provider.Artifact{Name: "base"}); err == nil {
		t.Error("server error must surface as an error")
	}
}

func TestEnsureBaseArtifactShortCircuitsWhenReady(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots/oi-x-abc":
			writeJSON(t, w, http.StatusOK, snapshotPayload("oi-x-abc", "active", ""))
		case r.Method == http.MethodPost:
			creates++
			http.Error(w, "must not create", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	artifact, err := newTestProvider(t, server.URL).EnsureBaseArtifact(
		context.Background(), provider.RuntimeBundle{ArtifactName: "oi-x-abc"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if creates != 0 {
		t.Errorf("ensure created a snapshot although one is active")
	}
	if artifact.ID != "id-oi-x-abc" {
		t.Errorf("artifact id = %q", artifact.ID)
	}
}

func TestDeleteBaseArtifactToleratesAbsence(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if err := newTestProvider(t, server.URL).DeleteBaseArtifact(
		context.Background(), provider.Artifact{Name: "gone"}); err != nil {
		t.Fatalf("delete absent artifact must be a no-op, got %v", err)
	}
}

func TestDeleteBaseArtifactPropagatesFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, snapshotPayload("base", "active", ""))
			return
		}
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	// A failed delete is unknown state, not success (fail closed).
	if err := newTestProvider(t, server.URL).DeleteBaseArtifact(
		context.Background(), provider.Artifact{Name: "base"}); err == nil {
		t.Error("failed delete must surface as an error")
	}
}

func TestTerminateSandboxToleratesAbsence(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if err := newTestProvider(t, server.URL).TerminateSandbox(context.Background(), "expired"); err != nil {
		t.Fatalf("terminate absent sandbox must be a no-op, got %v", err)
	}
}

func TestListOwnedSandboxesEmptyEvidenceListsNothing(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.NotFound(w, nil)
	}))
	defer server.Close()
	// No D1 session evidence means nothing can be owned — the provider must
	// not even be queried (fail closed, D8).
	owned, err := newTestProvider(t, server.URL).ListOwnedSandboxes(
		context.Background(), provider.OwnerMetadata{})
	if err != nil || owned != nil || calls != 0 {
		t.Errorf("owned=%v err=%v calls=%d, want nil/nil/0", owned, err, calls)
	}
}

func TestBuildImageReplaysManifestOrUsesOverride(t *testing.T) {
	p := newTestProvider(t, "https://unused.invalid")

	override, cleanup, err := p.buildImage(provider.RuntimeBundle{
		BaseImageOverride: "ghcr.io/dirien/openinspect-sandbox-runtime:v1",
		ArtifactName:      "x",
	})
	if err != nil {
		t.Fatalf("override image: %v", err)
	}
	defer cleanup()
	if df := override.Dockerfile(); df != "FROM ghcr.io/dirien/openinspect-sandbox-runtime:v1" {
		t.Errorf("override dockerfile = %q", df)
	}
	if len(override.Contexts()) != 0 {
		t.Errorf("override build must not upload a context")
	}
}
