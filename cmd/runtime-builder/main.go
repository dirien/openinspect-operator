// runtime-builder performs one provider base artifact build and exits. It
// runs inside the operator-owned runtime-build Job (EPIC1 section 6): the
// canonical runtime bundle is embedded in this binary, credentials arrive
// via the Job's environment (only the selected provider's Secret is
// mounted), and the process exits zero only after the remote artifact is
// usable. It is idempotent — an already-usable artifact is a no-op.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
	daytonaprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/daytona"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runtime-builder:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providerName := os.Getenv("OI_PROVIDER")
	artifactName := os.Getenv("OI_ARTIFACT_NAME")
	if artifactName == "" {
		return fmt.Errorf("OI_ARTIFACT_NAME is required")
	}

	manifest, err := sandboxruntime.Load()
	if err != nil {
		return fmt.Errorf("load runtime manifest: %w", err)
	}
	bundle := provider.RuntimeBundle{
		Manifest:          manifest,
		BaseImageOverride: os.Getenv("OI_RUNTIME_BASE_IMAGE"),
		ArtifactName:      artifactName,
	}

	logf := func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}
	logf("building %s base artifact %s (upstream %s)", providerName, artifactName, manifest.UpstreamCommit)

	var p provider.Provider
	switch providerName {
	case "daytona":
		p, err = daytonaprovider.New(daytonaprovider.Config{
			APIKey: os.Getenv("DAYTONA_API_KEY"),
			APIURL: os.Getenv("DAYTONA_API_URL"),
			Target: os.Getenv("DAYTONA_TARGET"),
			Logf:   logf,
		})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported provider %q", providerName)
	}

	artifact, err := p.EnsureBaseArtifact(ctx, bundle)
	if err != nil {
		return err
	}
	// Belt and braces: the Job's success is the controller's signal, so
	// re-verify usability instead of trusting EnsureBaseArtifact's return.
	state, err := p.InspectBaseArtifact(ctx, artifact)
	if err != nil {
		return err
	}
	if state.Phase != provider.ArtifactReady {
		return fmt.Errorf("artifact %s is %s after build: %s", artifact.Name, state.Phase, state.Message)
	}
	logf("artifact %s ready (id %s)", artifact.Name, artifact.ID)
	return nil
}
