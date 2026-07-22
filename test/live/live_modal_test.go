//go:build live

package live

import (
	"context"
	"os"
	"testing"
	"time"

	modalprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/modal"
)

// Modal live conformance is deliberately read-only against the configured
// workspace: it proves authentication, deployed-app endpoint discovery, and
// health — the surfaces the operator's reconcile loop depends on — without
// mutating an environment that may be a real installation. The deploy Job
// and secret convergence are exercised by the in-cluster flow against a
// dedicated environment, never implicitly from here.
//
//	OI_LIVE_MODAL=1 MODAL_TOKEN_ID=... MODAL_TOKEN_SECRET=... \
//	MODAL_WORKSPACE=... [MODAL_ENVIRONMENT=main] \
//	  go test -tags=live ./test/live/ -run Modal -v
func modalLive(t *testing.T) *modalprovider.Provider {
	t.Helper()
	if os.Getenv("OI_LIVE_MODAL") == "" {
		t.Skip("OI_LIVE_MODAL not set; skipping live Modal conformance")
	}
	p, err := modalprovider.New(modalprovider.Config{
		TokenID:     os.Getenv("MODAL_TOKEN_ID"),
		TokenSecret: os.Getenv("MODAL_TOKEN_SECRET"),
		Workspace:   os.Getenv("MODAL_WORKSPACE"),
		Environment: os.Getenv("MODAL_ENVIRONMENT"),
		Logf:        t.Logf,
	})
	if err != nil {
		t.Fatalf("modal config: %v", err)
	}
	return p
}

// TestModalDiscoverAndHealth resolves the deployed upstream app's endpoints
// and health-checks them, the exact drift check the reconciler performs.
// If no app is deployed in the environment, the typed ErrAppNotFound must
// surface — never a fabricated success.
func TestModalDiscoverAndHealth(t *testing.T) {
	p := modalLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	endpoints, err := p.DiscoverEndpoints(ctx)
	if err == modalprovider.ErrAppNotFound {
		t.Skipf("no %s app deployed in this environment; discovery contract (typed not-found) holds", modalprovider.AppName)
	}
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	t.Logf("discovered app %s healthURL=%s webSuffix=%q", endpoints.AppID, endpoints.HealthURL, endpoints.WebSuffix)
	if endpoints.HealthURL == "" {
		t.Fatal("empty health URL")
	}
	if err := modalprovider.HealthCheck(ctx, endpoints.HealthURL); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
}
