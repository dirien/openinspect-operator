package controller

import (
	"context"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	modalprovider "github.com/dirien/openinspect-operator/internal/sandbox/provider/modal"
)

// providerModal is the SANDBOX_PROVIDER value for Modal.
const providerModal = "modal"

// modalAppName is the fixed name used by the clean upstream Modal deployment.
const modalAppName = modalprovider.AppName

// modalDefaultEnvironment is Modal's default environment name.
const modalDefaultEnvironment = modalprovider.DefaultEnvironment

func modalEnvironment(m *openinspectv1alpha1.ModalSandboxSpec) string {
	if m.Environment == "" {
		return modalDefaultEnvironment
	}
	return m.Environment
}

// cleanupModalApp terminates every live sandbox in the CR's Modal app and then
// stops the app deployment, through the Modal adapter's authenticated API.
// appFound is false when no app exists under the spec's environment NAME —
// which the caller must surface: a wrong environment looks exactly like "no
// app" and would silently leak the real one.
func cleanupModalApp(
	ctx context.Context,
	m *openinspectv1alpha1.ModalSandboxSpec,
	tokenID, tokenSecret string,
) (terminated int, appFound bool, err error) {
	p, err := modalprovider.New(modalprovider.Config{
		TokenID:     tokenID,
		TokenSecret: tokenSecret,
		Workspace:   m.Workspace,
		Environment: modalEnvironment(m),
	})
	if err != nil {
		return 0, false, err
	}
	return p.TerminateAndStop(ctx)
}
