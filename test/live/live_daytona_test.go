//go:build live

// Package live holds the guarded, cost-limited provider conformance tests
// (EPIC1 section 9). They talk to REAL provider accounts and are therefore
// opt-in per provider:
//
//	OI_LIVE_DAYTONA=1 DAYTONA_API_KEY=... [DAYTONA_API_URL=...] \
//	  go test -tags=live ./test/live/ -run Daytona -v -timeout 45m
//
// Every test uses dedicated, uniquely named artifacts, deletes what it
// creates, and fails closed on unknown state.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
	daytonaprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/daytona"
)

func daytonaLive(t *testing.T) *daytonaprovider.Provider {
	t.Helper()
	if os.Getenv("OI_LIVE_DAYTONA") == "" {
		t.Skip("OI_LIVE_DAYTONA not set; skipping live Daytona conformance")
	}
	apiURL := os.Getenv("DAYTONA_API_URL")
	if apiURL == "" {
		apiURL = "https://app.daytona.io/api"
	}
	p, err := daytonaprovider.New(daytonaprovider.Config{
		APIKey: os.Getenv("DAYTONA_API_KEY"),
		APIURL: apiURL,
		Target: os.Getenv("DAYTONA_TARGET"),
		Logf:   t.Logf,
	})
	if err != nil {
		t.Fatalf("daytona config: %v", err)
	}
	return p
}

// TestDaytonaBaseArtifactLifecycle proves the managed-artifact contract
// against the real provider: build from the canonical runtime manifest,
// inspect to Ready, idempotent re-ensure, delete, confirm absence.
func TestDaytonaBaseArtifactLifecycle(t *testing.T) {
	p := daytonaLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	manifest, err := sandboxruntime.Load()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	short, err := manifest.ShortDigest()
	if err != nil {
		t.Fatal(err)
	}
	// Unique per run so parallel/aborted runs can never collide; the digest
	// keeps the name shape production-faithful.
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	name := "oi-live" + hex.EncodeToString(nonce) + "-" + short
	bundle := provider.RuntimeBundle{Manifest: manifest, ArtifactName: name}

	// Always clean up, even on failure paths.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := p.DeleteBaseArtifact(cleanupCtx, provider.Artifact{Name: name}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	t.Logf("building live artifact %s (upstream %s)", name, manifest.UpstreamCommit)
	artifact, err := p.EnsureBaseArtifact(ctx, bundle)
	if err != nil {
		t.Fatalf("EnsureBaseArtifact: %v", err)
	}
	if artifact.Name != name {
		t.Fatalf("artifact name = %q, want %q", artifact.Name, name)
	}

	state, err := p.InspectBaseArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("InspectBaseArtifact: %v", err)
	}
	if state.Phase != provider.ArtifactReady {
		t.Fatalf("artifact phase = %s (%s), want Ready", state.Phase, state.Message)
	}

	// Idempotence: a second ensure must return immediately without a build.
	start := time.Now()
	if _, err := p.EnsureBaseArtifact(ctx, bundle); err != nil {
		t.Fatalf("idempotent EnsureBaseArtifact: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Minute {
		t.Errorf("re-ensure took %s; expected a short-circuit on the existing artifact", elapsed)
	}

	// Delete and confirm absence — unknown state is never success.
	if err := p.DeleteBaseArtifact(ctx, artifact); err != nil {
		t.Fatalf("DeleteBaseArtifact: %v", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		state, err := p.InspectBaseArtifact(ctx, artifact)
		if err != nil {
			t.Fatalf("inspect after delete: %v", err)
		}
		if state.Phase == provider.ArtifactAbsent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("artifact still %s after delete", state.Phase)
		}
		time.Sleep(5 * time.Second)
	}
}

// TestDaytonaOwnershipListingIsScoped proves the D8 fail-closed contract
// live: with no D1 session evidence nothing is listed, and with fabricated
// evidence nothing foreign matches.
func TestDaytonaOwnershipListingIsScoped(t *testing.T) {
	p := daytonaLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	owned, err := p.ListOwnedSandboxes(ctx, provider.OwnerMetadata{})
	if err != nil {
		t.Fatalf("empty evidence: %v", err)
	}
	if len(owned) != 0 {
		t.Fatalf("no evidence must own nothing, got %d", len(owned))
	}

	fabricated := provider.OwnerMetadata{SessionIDs: map[string]struct{}{
		"session-that-cannot-exist-" + hex.EncodeToString([]byte(time.Now().String()))[:12]: {},
	}}
	owned, err = p.ListOwnedSandboxes(ctx, fabricated)
	if err != nil {
		t.Fatalf("fabricated evidence: %v", err)
	}
	if len(owned) != 0 {
		t.Fatalf("fabricated session IDs matched %d sandboxes; ownership intersection is broken", len(owned))
	}
}
