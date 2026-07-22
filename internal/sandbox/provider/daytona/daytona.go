// Package daytona implements the provider contract with the official
// Daytona Go SDK: the managed base snapshot is built declaratively from the
// canonical runtime manifest, and finalization deletes only sandboxes whose
// session labels are proven by CR-owned D1 evidence (EPIC1 D8).
package daytona

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	daytonaerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"

	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

// Upstream sandbox labels (set by the unmodified control plane).
const (
	FrameworkLabel      = "openinspect_framework"
	FrameworkLabelValue = "open-inspect"
	SessionIDLabel      = "openinspect_session_id"
)

// maxListItems bounds the sandbox listing: a cursor pager that never
// terminates must not hang a reconcile worker or the finalizer forever.
const maxListItems = 10000

// Snapshot deletion is asynchronous; recreating the same name inside the
// window fails with "already exists", so ensure-after-failure waits for the
// delete to become visible (mirrors upstream bootstrap.py).
const (
	deleteVisibleTimeout = 5 * time.Minute
	pollInterval         = 5 * time.Second
)

// Config carries explicit credentials and endpoints; nothing is read from
// process environment, so one CR can never use another CR's credentials.
type Config struct {
	APIKey string
	// APIURL is required: the SDK falls back to its own env vars / public
	// default for an empty URL, which would target the wrong deployment.
	APIURL string
	Target string
	// Logf receives build progress lines; nil discards them. Never log
	// credentials through it.
	Logf func(format string, args ...any)
}

// Provider implements provider.Provider for Daytona.
type Provider struct {
	cfg Config
}

var _ provider.Provider = (*Provider)(nil)

// New validates the configuration and returns the adapter.
func New(cfg Config) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("daytona: api key is empty")
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("daytona: api url is empty (refusing the SDK's fallback endpoint)")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Provider{cfg: cfg}, nil
}

func (p *Provider) client() (*daytona.Client, error) {
	usePolling := true
	client, err := daytona.NewClientWithConfig(&daytonatypes.DaytonaConfig{
		APIKey:               p.cfg.APIKey,
		APIUrl:               p.cfg.APIURL,
		Target:               p.cfg.Target,
		UseDeprecatedPolling: &usePolling,
	})
	if err != nil {
		return nil, fmt.Errorf("create daytona client: %w", err)
	}
	return client, nil
}

func isNotFound(err error) bool {
	var notFound *daytonaerrors.DaytonaNotFoundError
	return errors.As(err, &notFound)
}

// phase maps Daytona snapshot states onto the provider-neutral phases.
// Inactive snapshots reactivate on use, so they count as Ready. Unknown
// states (including the OpenAPI unknown-enum sentinel for states newer than
// the pinned SDK) map to ok=false: unknown is neither buildable-over nor
// deletable — callers must fail closed and retry.
func phase(state string) (provider.ArtifactPhase, bool) {
	switch state {
	case "active", "inactive":
		return provider.ArtifactReady, true
	case "error", "build_failed":
		return provider.ArtifactFailed, true
	case "pending", "building", "pulling", "removing":
		return provider.ArtifactBuilding, true
	default:
		return "", false
	}
}

// InspectBaseArtifact reports the remote snapshot state.
func (p *Provider) InspectBaseArtifact(ctx context.Context, artifact provider.Artifact) (provider.ArtifactState, error) {
	client, err := p.client()
	if err != nil {
		return provider.ArtifactState{}, err
	}
	defer func() { _ = client.Close(ctx) }()
	return p.inspect(ctx, client, artifact.Name)
}

func (p *Provider) inspect(ctx context.Context, client *daytona.Client, name string) (provider.ArtifactState, error) {
	snap, err := client.Snapshot.Get(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return provider.ArtifactState{Phase: provider.ArtifactAbsent}, nil
		}
		return provider.ArtifactState{}, fmt.Errorf("get daytona snapshot %q: %w", name, err)
	}
	mapped, known := phase(snap.State)
	if !known {
		return provider.ArtifactState{}, fmt.Errorf("daytona snapshot %q is in unrecognized state %q: refusing to act on unknown state", name, snap.State)
	}
	state := provider.ArtifactState{Phase: mapped, ID: snap.ID}
	if snap.ErrorReason != nil {
		state.Message = *snap.ErrorReason
	}
	return state, nil
}

// EnsureBaseArtifact makes the named snapshot exist and be usable. A failed
// previous build under the same name is deleted and rebuilt; an in-progress
// build is awaited. Blocking is by design: this runs inside the
// runtime-build Job, not a reconcile worker.
func (p *Provider) EnsureBaseArtifact(ctx context.Context, bundle provider.RuntimeBundle) (provider.Artifact, error) {
	client, err := p.client()
	if err != nil {
		return provider.Artifact{}, err
	}
	defer func() { _ = client.Close(ctx) }()
	name := bundle.ArtifactName

	state, err := p.inspect(ctx, client, name)
	if err != nil {
		return provider.Artifact{}, err
	}
	switch state.Phase {
	case provider.ArtifactReady:
		p.cfg.Logf("snapshot %s already usable", name)
		return provider.Artifact{Name: name, ID: state.ID}, nil
	case provider.ArtifactBuilding:
		p.cfg.Logf("snapshot %s build already in progress; waiting", name)
		return p.awaitReady(ctx, client, name)
	case provider.ArtifactFailed:
		p.cfg.Logf("snapshot %s previously failed (%s); deleting and rebuilding", name, state.Message)
		if err := p.deleteAndWait(ctx, client, name); err != nil {
			return provider.Artifact{}, err
		}
	case provider.ArtifactAbsent:
		// build below
	}

	image, cleanup, err := p.buildImage(bundle)
	if err != nil {
		return provider.Artifact{}, err
	}
	defer cleanup()

	// The API rejects the entrypoint FIELD together with a build request
	// ("Cannot specify an entrypoint when using a build info entry"); for
	// declarative builds the entrypoint travels as a Dockerfile ENTRYPOINT
	// instruction inside the image instead (buildImage sets it).
	params := &daytonatypes.CreateSnapshotParams{Name: name, Image: image}
	if bundle.BaseImageOverride != "" {
		// Plain image reference: no build info, the field is accepted.
		params.Image = bundle.BaseImageOverride
		params.Entrypoint = bundle.Manifest.Entrypoint
	}
	p.cfg.Logf("creating snapshot %s", name)
	snap, logs, err := client.Snapshot.Create(ctx, params)
	if err != nil {
		return provider.Artifact{}, fmt.Errorf("create daytona snapshot %q: %w", name, err)
	}
	for line := range logs {
		p.cfg.Logf("%s", line)
	}

	// The log stream ending does not prove success: re-inspect and require
	// Ready before reporting the artifact usable.
	final, err := p.inspect(ctx, client, name)
	if err != nil {
		return provider.Artifact{}, err
	}
	if final.Phase == provider.ArtifactBuilding {
		return p.awaitReady(ctx, client, name)
	}
	if final.Phase != provider.ArtifactReady {
		return provider.Artifact{}, fmt.Errorf("daytona snapshot %q is %s after build: %s", name, final.Phase, final.Message)
	}
	id := final.ID
	if id == "" && snap != nil {
		id = snap.ID
	}
	return provider.Artifact{Name: name, ID: id}, nil
}

// buildImage maps the canonical manifest onto Daytona's declarative image
// DSL. With a base override, the prebuilt runtime image already carries the
// toolchain, files, and environment — only the FROM matters (the caller
// passes the reference as a plain image, bypassing the builder).
func (p *Provider) buildImage(bundle provider.RuntimeBundle) (*daytona.DockerImage, func(), error) {
	m := bundle.Manifest
	if bundle.BaseImageOverride != "" {
		return daytona.Base(bundle.BaseImageOverride), func() {}, nil
	}

	img := daytona.Base(m.BaseImage)
	for _, step := range m.Steps {
		for _, cmd := range step.Commands {
			img = img.Run(cmd)
		}
	}
	for _, k := range sortedKeys(m.Env) {
		img = img.Env(k, m.Env[k])
	}
	version, err := m.SandboxVersion()
	if err != nil {
		return nil, nil, err
	}
	img = img.Env("SANDBOX_VERSION", version)

	// The declarative build context uploads local files; materialize the
	// embedded runtime tree for it.
	dir, err := os.MkdirTemp("", "oi-runtime-*")
	if err != nil {
		return nil, nil, err
	}
	if err := m.Materialize(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("materialize runtime tree: %w", err)
	}
	// Entrypoint as a Dockerfile instruction: the create API refuses the
	// entrypoint field for build requests.
	img = img.AddLocalDir(dir, m.RuntimeDir).Workdir(m.WorkDir).Entrypoint(m.Entrypoint)
	return img, func() { _ = os.RemoveAll(dir) }, nil
}

func (p *Provider) awaitReady(ctx context.Context, client *daytona.Client, name string) (provider.Artifact, error) {
	for {
		state, err := p.inspect(ctx, client, name)
		if err != nil {
			return provider.Artifact{}, err
		}
		switch state.Phase {
		case provider.ArtifactReady:
			return provider.Artifact{Name: name, ID: state.ID}, nil
		case provider.ArtifactFailed:
			return provider.Artifact{}, fmt.Errorf("daytona snapshot %q build failed: %s", name, state.Message)
		case provider.ArtifactAbsent:
			return provider.Artifact{}, fmt.Errorf("daytona snapshot %q disappeared while waiting for the build", name)
		case provider.ArtifactBuilding:
		}
		select {
		case <-ctx.Done():
			return provider.Artifact{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// DeleteBaseArtifact removes the snapshot, tolerating absence.
func (p *Provider) DeleteBaseArtifact(ctx context.Context, artifact provider.Artifact) error {
	client, err := p.client()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close(ctx) }()
	snap, err := client.Snapshot.Get(ctx, artifact.Name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get daytona snapshot %q: %w", artifact.Name, err)
	}
	if err := client.Snapshot.Delete(ctx, snap); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete daytona snapshot %q: %w", artifact.Name, err)
	}
	return nil
}

// deleteAndWait deletes the named snapshot and blocks until the deletion is
// visible, so an immediate recreate under the same name cannot collide.
func (p *Provider) deleteAndWait(ctx context.Context, client *daytona.Client, name string) error {
	snap, err := client.Snapshot.Get(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get daytona snapshot %q: %w", name, err)
	}
	if err := client.Snapshot.Delete(ctx, snap); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete daytona snapshot %q: %w", name, err)
	}
	deadline := time.Now().Add(deleteVisibleTimeout)
	for {
		if _, err := client.Snapshot.Get(ctx, name); err != nil {
			if isNotFound(err) {
				return nil
			}
			return fmt.Errorf("get daytona snapshot %q: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daytona snapshot %q still present %s after delete", name, deleteVisibleTimeout)
		}
		p.cfg.Logf("waiting for snapshot %s deletion to become visible", name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// ListOwnedSandboxes lists the CR's sandboxes: framework-labeled sandboxes
// whose session label occurs in the D1-backed ownership evidence. Labels
// are rechecked locally even though Daytona filters server-side, keeping
// deletion fail-closed if a proxy or older API ignores the query.
func (p *Provider) ListOwnedSandboxes(ctx context.Context, owner provider.OwnerMetadata) ([]provider.Sandbox, error) {
	if len(owner.SessionIDs) == 0 {
		return nil, nil
	}
	client, err := p.client()
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close(ctx) }()

	// Finish the cursor-paginated listing BEFORE anything is deleted:
	// deleting mid-iteration shifts pages under an offset-style cursor and
	// skips sandboxes that were owned.
	iter := client.List(ctx, &daytona.ListSandboxesQuery{
		Labels: map[string]string{FrameworkLabel: FrameworkLabelValue},
	})
	var owned []provider.Sandbox
	for n := 0; iter.Next(); n++ {
		if n > maxListItems {
			return nil, fmt.Errorf("list daytona sandboxes: pagination never terminated")
		}
		sandbox := iter.Value()
		if sandbox.Labels[FrameworkLabel] != FrameworkLabelValue {
			continue
		}
		sessionID := sandbox.Labels[SessionIDLabel]
		if _, own := owner.SessionIDs[sessionID]; !own {
			continue
		}
		owned = append(owned, provider.Sandbox{ID: sandbox.ID, SessionID: sessionID})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list daytona sandboxes: %w", err)
	}
	return owned, nil
}

// TerminateSandbox deletes one sandbox, tolerating absence.
func (p *Provider) TerminateSandbox(ctx context.Context, id string) error {
	client, err := p.client()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close(ctx) }()
	sandbox, err := client.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("get daytona sandbox %q: %w", id, err)
	}
	if err := sandbox.Delete(ctx); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete daytona sandbox %q: %w", id, err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
