# Adding a Sandbox Provider

## Purpose

This guide describes the work required to add a sandbox provider to the
OpenInspect operator. A provider is not considered supported merely because
its name is accepted by the CRD or its credentials are injected into a
Cloudflare Worker. Support means the operator can manage the provider safely
through the complete resource lifecycle.

The only fully supported providers today are:

| Provider | Configuration | Sandbox cleanup | App/base-resource cleanup |
|---|---|---|---|
| Modal | Supported | Supported through the Modal Go SDK | Modal app stopped through Modal's authenticated API |
| Daytona | Supported | Supported through the Daytona Go SDK | Sandboxes deleted using D1-backed ownership evidence |

The API currently contains some Vercel and OpenComputer configuration
scaffolding. These providers must not be advertised as supported until they
satisfy the definition of done in this document.

## Non-negotiable implementation rules

Every new provider integration must follow these rules:

- Implement provider operations in this repository using an official Go SDK
  or a typed Go HTTP client.
- Do not use Terraform, provider CLIs, shell commands, or subprocesses.
- Do not patch or require changes to the upstream
  `ColeMurray/background-agents` repository.
- Do not depend on private HTTP endpoints added to a local upstream checkout.
- Never log provider tokens, request environments, sandbox environment
  variables, or Secret values.
- Make every ensure and delete operation idempotent.
- Treat a timeout, DNS error, network error, rate limit, or unknown provider
  state as a retryable failure. Unknown state is not successful deletion.
- Keep the Kubernetes finalizer until all owned external resources are
  confirmed absent.
- Make `spec.deletionPolicy: Orphan` the only way to intentionally skip
  external cleanup.

## What “operator-managed” means

The target architecture is defined in [EPIC1.md](EPIC1.md). Provider calls
will be made by the operator-owned `sandbox-api`, using adapters implemented
in this repository. Cloudflare Workers will call that API and will not receive
provider credentials.

Modal and Daytona currently have a compatibility path in which the upstream
control-plane Worker creates sandboxes while the operator owns finalization.
Do not copy that split ownership as the permanent design for a new provider.
A new provider should implement the managed adapter and `sandbox-api` path.

If a provider must temporarily use the compatibility path, the pinned,
unmodified upstream control plane must already support it. Otherwise, complete
the operator-owned `sandbox-api` work first. Modifying upstream is not an
acceptable integration strategy.

## Required provider capabilities

Before writing code, make a capability matrix for the provider. Record the
SDK or API operation used for every row and identify unsupported operations.

| Capability | Requirement |
|---|---|
| Authentication | Credentials can be supplied directly to an SDK/client without global process state |
| Base artifact | Build, find, inspect, version, and delete an image, snapshot, or template |
| Create | Create a sandbox from the selected base artifact |
| Restore | Restore or recreate a sandbox from a snapshot |
| Resume | Resume a stopped or paused sandbox, or document an idempotent recreate equivalent |
| Execute/bootstrap | Start the OpenInspect runtime without a provider CLI |
| Snapshot | Create and inspect a filesystem snapshot |
| Stop/terminate | Stop a sandbox idempotently |
| List | List sandboxes using ownership metadata or safely filter them locally |
| Delete | Delete sandboxes and provider artifacts idempotently |
| Metadata | Attach and retrieve OpenInspect ownership labels/tags |
| State | Distinguish absent, pending, ready, failed, and unknown states |
| Pagination | Traverse all pages with a bounded safety limit |
| Rate limiting | Expose retry information without busy-looping |

If the provider cannot attach and query ownership metadata, it is not safe for
managed multi-tenant cleanup without a separate durable ownership index.

## Ownership contract

Every resource created for a CR should carry these values, using the closest
provider equivalent to labels or tags:

```text
openinspect.uid
openinspect.namespace
openinspect.name
openinspect.session_id
openinspect.sandbox_id
openinspect.runtime_digest
```

The CR UID is the primary tenant boundary. Namespace and name are useful for
operators, but are not sufficient because they can be reused after deletion.

For compatibility with sandboxes created by older control-plane bundles,
Daytona intersects the provider's `openinspect_session_id` label with IDs from
the CR-owned D1 `sessions` table. This is a migration fallback, not the ideal
ownership model. Never delete resources using only a shared marker such as
`openinspect_framework=open-inspect`.

The cleanup order must preserve ownership evidence:

1. Read durable ownership records.
2. List and delete owned provider sandboxes.
3. Delete owned provider images, snapshots, templates, apps, and other shared
   resources when no live CR still uses them.
4. Delete the Cloudflare Worker and D1 database.
5. Remove the Kubernetes finalizer.

If ownership cannot be proven, fail closed. Return an error, emit a warning
Event, retain the finalizer, and require an explicit `Orphan` policy or an
operator-approved recovery procedure.

## Implementation procedure

### 1. Add the provider SDK or typed client

Prefer the official Go SDK when it exposes the required lifecycle. Pin an
exact released version in `go.mod`. If no suitable SDK exists, add a small,
typed HTTP client under the provider adapter package with:

- explicit base URL and credentials;
- context-aware requests and finite timeouts;
- bounded response bodies;
- typed request, response, state, and error models;
- pagination limits;
- retry classification for `429` and transient `5xx` responses;
- tests using `httptest.Server`.

Do not read credentials implicitly from environment variables when they can
be passed directly. This prevents one reconciled CR from using another CR's
credentials.

### 2. Define the CRD configuration

Add `<Provider>SandboxSpec` to
`api/v1alpha1/openinspect_types.go`. Configuration should contain provider
options and `SecretKeyRef` values, not Secret contents.

Prefer this shape:

```go
type ExampleSandboxSpec struct {
	APIURL          string       `json:"apiUrl,omitempty"`
	Region          string       `json:"region,omitempty"`
	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`
}
```

Then:

- add the provider to the `SandboxSpec.Provider` kubebuilder enum;
- add exactly one optional provider section to `SandboxSpec`;
- add defaults and validation markers where the provider has stable limits;
- avoid making a manually prebuilt image or snapshot mandatory for the
  managed path;
- add runtime status and `metav1.Condition` values if the provider introduces
  an asynchronous artifact build.

Do not edit generated CRDs or `zz_generated` files manually. Run:

```sh
make manifests
make generate
```

### 3. Register validation

Update `internal/controller/sandbox.go` so:

- the provider value requires its matching configuration section;
- configuration for a different provider is rejected;
- required cross-field values are validated before any external API call;
- the configured control-plane Worker must exist;
- unsupported or mutually exclusive options produce an `InvalidSpec`
  condition rather than repeated API retries.

Add table-driven tests to `internal/controller/sandbox_test.go` for:

- valid minimal configuration;
- missing provider section;
- mismatched provider section;
- multiple provider sections;
- missing Secret references or required provider options;
- defaulted optional fields.

### 4. Watch every referenced Secret

Add all provider `SecretKeyRef` fields to `referencesSecret` in
`internal/controller/watches.go`. A Secret update must enqueue every CR in the
same namespace that references it.

Add tests to `internal/controller/watches_test.go` covering positive and
negative matches. Never grant cross-namespace Secret access.

### 5. Implement the managed provider adapter

Implement the shared provider lifecycle described in EPIC 1 under:

```text
internal/sandbox/provider/<provider>/
```

The adapter must cover:

```go
type Provider interface {
	EnsureBaseArtifact(context.Context, RuntimeBundle) (Artifact, error)
	InspectBaseArtifact(context.Context, Artifact) (ArtifactState, error)
	DeleteBaseArtifact(context.Context, Artifact) error

	CreateSandbox(context.Context, CreateRequest) (Sandbox, error)
	RestoreSandbox(context.Context, RestoreRequest) (Sandbox, error)
	SnapshotSandbox(context.Context, SnapshotRequest) (Snapshot, error)
	ResumeSandbox(context.Context, ResumeRequest) (Sandbox, error)
	StopSandbox(context.Context, StopRequest) error
	DeleteSnapshot(context.Context, string) error
	ListOwnedSandboxes(context.Context, OwnerMetadata) ([]Sandbox, error)
}
```

Keep provider-specific API types inside the adapter. The controller and
`sandbox-api` should consume normalized types and must not depend on raw SDK
responses.

If the provider builds base artifacts asynchronously, reconcile an owned
Kubernetes Job as described in EPIC 1. Do not block a reconciliation worker
for the full remote build duration.

### 6. Wire sandbox-api creation and credentials

For the managed path:

- register the adapter in `cmd/sandbox-api`;
- mount or project only the selected provider's credentials;
- disable the pod's Kubernetes service-account token;
- keep provider credentials out of the Cloudflare Worker;
- expose the same provider-neutral `/v1` request and response contract;
- return normalized retryable, terminal, conflict, and not-found errors;
- include CR UID and session ownership metadata on every create operation.

The provider must work through any supported external exposure mode: standard
Ingress, externally managed URL, or optional Gateway API. It must not depend
on a specific ingress vendor.

### 7. Add compatibility injection only when required

If the clean pinned upstream control plane already supports the provider and
a temporary compatibility path is necessary, add its plain variables and
Secret bindings to `sandboxInjection` in
`internal/controller/sandbox.go`.

Follow these rules:

- `SANDBOX_PROVIDER` must equal the canonical provider enum value;
- explicit Worker variables and bindings continue to win;
- Secret values remain Kubernetes Secret bindings and are never copied into
  the CR status;
- environment variable names must match the clean pinned upstream contract;
- add injection and precedence tests to
  `internal/controller/sandbox_test.go`.

Do not add compatibility injection for a provider that clean upstream does
not support. Use the operator-owned `sandbox-api` path instead.

### 8. Implement finalization

Add a provider cleanup implementation next to the existing examples:

```text
internal/controller/modalsweep.go
internal/controller/daytonasweep.go
internal/controller/<provider>sweep.go
```

Then register it in `cleanupProviderSandboxes` in
`internal/controller/openinspect_controller.go`.

Finalization must:

- obtain credentials from the CR's referenced Secret;
- load ownership evidence before deleting D1;
- list every page of provider resources;
- recheck ownership metadata locally even if the provider API filters it;
- tolerate resources already being absent;
- delete all owned sandboxes and managed artifacts;
- return any unknown or transient error so reconciliation retries;
- emit Kubernetes Events without including credentials;
- avoid deleting shared resources while another live CR references them.

Add a swappable cleanup function or client interface to the reconciler so
envtest does not contact a real provider.

### 9. Add status and conditions

Managed artifact reconciliation should publish stable, provider-neutral
status rather than raw SDK objects. At minimum record:

- provider;
- artifact ID and name;
- runtime digest and source revision;
- phase;
- observed generation;
- last successful build time;
- `RuntimeReady`, `SandboxAPIReady`, and `SandboxEndpointReady` conditions.

Never place access tokens, signed URLs, sandbox authentication tokens, or
sandbox environment variables in status, Events, or logs.

### 10. Add samples and installation wiring

Add a runnable sample:

```text
config/samples/openinspect_v1alpha1_openinspect_<provider>.yaml
```

Also update:

- `config/samples/kustomization.yaml`;
- Secret creation examples under `hack/` without real values;
- README provider and deletion documentation;
- Helm values only if the provider needs operator or `sandbox-api` deployment
  configuration;
- RBAC markers if new Kubernetes resource kinds are reconciled.

After marker or deployment changes, regenerate and synchronize distribution
artifacts:

```sh
make manifests generate
make helm-sync
```

## Testing requirements

### Unit tests

Unit tests must cover:

- request and response mapping;
- authentication headers without asserting or printing the Secret value;
- pagination and its upper bound;
- ownership filtering and local label rechecks;
- already-absent deletion;
- partial deletion followed by an idempotent retry;
- `429`, transient `5xx`, timeout, DNS, and malformed-response errors;
- terminal authentication and invalid-request errors;
- base-artifact state transitions;
- provider configuration injection and explicit-value precedence.

### Envtest

Envtest must prove:

- the Secret watch enqueues the CR;
- status and conditions advance correctly;
- deleting the CR invokes the provider cleanup hook;
- provider failure retains the finalizer;
- restoring credentials or provider health allows deletion to finish;
- `deletionPolicy: Orphan` skips provider calls;
- shared resources are not deleted while another CR uses them.

### Kind test with a mock provider

Add a mock provider API that supports create, list, snapshot, restore, stop,
delete, and injected failures. The Kind test must run in the isolated Kind
cluster required by `AGENTS.md`.

### Guarded live conformance test

A cost-limited live test must:

1. Apply one CR.
2. Wait for its runtime artifact and endpoint to become ready.
3. Create a sandbox.
4. Verify the runtime bridge.
5. Execute a command.
6. Snapshot and restore the sandbox.
7. Stop the sandbox.
8. Delete the CR.
9. Query the provider and confirm that no resources tagged with the CR UID
   remain.

Live tests must use a dedicated account/project, strict timeout, sandbox
auto-expiration, and explicit cost limits.

## Pull request sequence

For a provider larger than a small SDK addition, use this sequence:

1. Capability matrix, typed client, normalized error model, and unit tests.
2. CRD configuration, validation, Secret watches, and regenerated manifests.
3. Base-artifact build and inspection with status conditions.
4. Provider adapter and `sandbox-api` lifecycle operations.
5. Ownership metadata and finalizer cleanup.
6. Mock-provider Kind conformance test.
7. Sample CR, documentation, Helm synchronization, and guarded live test.

Do not merge a CRD enum value in isolation. Once users can select a provider,
the corresponding lifecycle and cleanup must already be safe.

## Definition of done

A provider is supported only when all of the following are true:

- One CR and Kubernetes Secret references are sufficient to configure it.
- No upstream patch, Terraform, CLI, or manual provider deployment is needed.
- The operator can build or reconcile its base artifact through code.
- Sandbox create, restore, resume, snapshot, stop, and delete work through the
  provider adapter.
- Every provider resource contains CR UID ownership metadata.
- Reconciliation is idempotent after process restarts and partial failures.
- Unknown provider state retains the finalizer.
- `Orphan` is explicit and does not occur accidentally because credentials
  are missing.
- Unit, envtest, Kind mock, and guarded live conformance tests pass.
- Generated CRDs, RBAC, Helm chart, samples, and documentation are current.
- `make lint` and `make test` pass.

## Final checklist

```text
[ ] Official SDK or typed HTTP client; no CLI/Terraform/subprocess
[ ] No upstream changes or private upstream endpoints
[ ] CRD provider spec, enum, markers, defaults, and status
[ ] Exactly-one-provider validation
[ ] Secret watch mapping
[ ] Managed provider adapter and sandbox-api registration
[ ] Ownership labels include CR UID
[ ] Base-artifact ensure/inspect/delete
[ ] Sandbox create/restore/resume/snapshot/stop
[ ] Idempotent, ownership-safe finalizer cleanup
[ ] Unknown health retains finalizer
[ ] Orphan policy tested
[ ] Unit, envtest, Kind mock, and live conformance coverage
[ ] Sample CR and Secret documentation
[ ] make manifests generate
[ ] make helm-sync
[ ] make lint
[ ] make test
```
