// Package provider defines the operator-side sandbox provider contract:
// base artifacts and finalization only (EPIC1 D4). Session sandboxes are
// created, snapshotted, resumed, and stopped by the unmodified upstream
// control plane — never through this interface.
package provider

import (
	"context"

	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
)

// RuntimeBundle is the input of a base artifact build.
type RuntimeBundle struct {
	// Manifest is the canonical runtime recipe (internal/runtime).
	Manifest *sandboxruntime.Manifest
	// BaseImageOverride, when set (spec.sandbox.runtime.image), makes the
	// artifact build FROM this prebuilt runtime image instead of replaying
	// the manifest's recipe steps.
	BaseImageOverride string
	// ArtifactName is the deterministic, digest-derived name the artifact
	// must be created under.
	ArtifactName string
}

// Artifact identifies one provider base artifact.
type Artifact struct {
	Name string
	// ID is the provider-assigned identifier, when one exists.
	ID string
}

// ArtifactPhase classifies remote artifact state.
type ArtifactPhase string

const (
	// ArtifactAbsent: the artifact does not exist.
	ArtifactAbsent ArtifactPhase = "Absent"
	// ArtifactBuilding: the provider is still building or transferring it.
	ArtifactBuilding ArtifactPhase = "Building"
	// ArtifactReady: the artifact is usable for sandbox creation.
	ArtifactReady ArtifactPhase = "Ready"
	// ArtifactFailed: the provider reports a terminal build failure.
	ArtifactFailed ArtifactPhase = "Failed"
)

// ArtifactState is the inspected remote state of an artifact.
type ArtifactState struct {
	Phase ArtifactPhase
	// ID is the provider-assigned identifier when known.
	ID string
	// Message carries provider detail (e.g. the build error).
	Message string
}

// Sandbox is one provider sandbox owned by a CR (per D8 ownership evidence).
type Sandbox struct {
	ID        string
	SessionID string
}

// OwnerMetadata carries the ownership evidence for finalization: the session
// IDs read from the CR-owned D1 sessions table. A sandbox is owned only when
// its openinspect_session_id label occurs in this set (EPIC1 D8).
type OwnerMetadata struct {
	SessionIDs map[string]struct{}
}

// Provider is the operator-side contract every supported sandbox provider
// implements.
type Provider interface {
	// EnsureBaseArtifact makes the named artifact exist and be usable,
	// building it when absent. It blocks until the remote artifact is
	// usable or the build terminally fails, streaming progress through the
	// bundle's build; it runs inside the runtime-build Job, never inside a
	// reconcile worker. It is idempotent: an existing usable artifact
	// returns immediately.
	EnsureBaseArtifact(ctx context.Context, bundle RuntimeBundle) (Artifact, error)
	// InspectBaseArtifact reports the remote state of the artifact.
	InspectBaseArtifact(ctx context.Context, artifact Artifact) (ArtifactState, error)
	// DeleteBaseArtifact removes the artifact, tolerating absence.
	DeleteBaseArtifact(ctx context.Context, artifact Artifact) error

	// ListOwnedSandboxes returns the sandboxes proven to belong to the CR
	// (finalization only, D4; ownership per D8).
	ListOwnedSandboxes(ctx context.Context, owner OwnerMetadata) ([]Sandbox, error)
	// TerminateSandbox terminates/deletes one sandbox, tolerating absence.
	TerminateSandbox(ctx context.Context, id string) error
}
