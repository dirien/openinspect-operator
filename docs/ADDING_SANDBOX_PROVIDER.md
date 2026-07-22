# Adding a Sandbox Provider

## Purpose

This guide describes the work required to add a sandbox provider to the
OpenInspect operator. A provider is not considered supported merely because
its name is accepted by the CRD or its credentials are injected into a
Cloudflare Worker. Support means the operator can install the provider's
base artifacts and finalize its resources safely through the complete CR
lifecycle.

The only fully supported providers today are:

| Provider | Configuration | Base artifact | Sandbox cleanup | App/base-resource cleanup |
|---|---|---|---|---|
| Modal | Supported | Built by the deployed upstream app | Modal Go SDK | App deployed by the modal-deploy Job, stopped through Modal's authenticated API |
| Daytona | Supported | Managed snapshot built by the runtime-build Job (Daytona Go SDK) | Daytona Go SDK with D1-backed ownership evidence | Managed snapshot deleted on CR deletion |

The API currently contains some Vercel and OpenComputer configuration
scaffolding. These providers must not be advertised as supported until the
pinned upstream revision actually drives them and they satisfy the
definition of done in this document (EPIC1 D3).

## What the operator is (and is not)

The operator is an **installer and lifecycle manager for unmodified
upstream** ([EPIC1.md](EPIC1.md) D1, D4). Upstream's control plane makes all
provider calls at runtime — creating, snapshotting, resuming, and stopping
session sandboxes. The operator:

- builds, inspects, and deletes provider **base artifacts**;
- deploys provider-side applications where upstream requires one (Modal);
- terminates **owned** sandboxes during finalization;
- never sits in the session path and never proxies provider APIs.

There is no operator-owned sandbox API. Do not design one.

## Non-negotiable implementation rules

Every new provider integration must follow these rules:

- Implement provider operations in this repository using an official Go SDK
  or a typed Go HTTP client.
- Do not use Terraform, provider CLIs, shell commands, or subprocesses.
  **The single scoped exception (EPIC1 D5)** is the operator-owned
  modal-deploy Job: Modal Functions cannot be deployed through the Go SDK,
  so that Job runs the pinned Modal CLI against the pinned upstream source.
  Nothing else may shell out, and no new provider may claim this exception
  without an equivalent hard SDK limitation recorded in EPIC1.
- Do not patch or require changes to the upstream
  `ColeMurray/background-agents` repository (EPIC1 D2).
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

## Required provider capabilities

Before writing code, make a capability matrix for the provider. Record the
SDK or API operation used for every row and identify unsupported operations.
Session-sandbox lifecycle (create, restore, resume, snapshot, stop) is
**upstream's** concern: verify the pinned upstream control plane drives the
provider, but do not build operator code for it.

| Capability | Requirement |
|---|---|
| Authentication | Credentials can be supplied directly to an SDK/client without global process state |
| Base artifact | Build, find, inspect, version, and delete an image, snapshot, or template |
| List | List sandboxes using ownership metadata or safely filter them locally |
| Terminate | Terminate/delete a sandbox idempotently (finalization only) |
| Metadata | Retrieve the ownership labels upstream attaches to sandboxes |
| State | Distinguish absent, pending, ready, failed, and unknown states |
| Pagination | Traverse all pages with a bounded safety limit |
| Rate limiting | Expose retry information without busy-looping |

If the provider cannot attach and query ownership metadata, it is not safe
for managed multi-tenant cleanup without a separate durable ownership index.

## Ownership contract

Unmodified upstream labels every sandbox it creates with:

```text
openinspect_framework=open-inspect
openinspect_session_id=<session id>
```

`openinspect_framework` alone is never ownership evidence — every
Open-Inspect installation shares it. Ownership is established permanently
(EPIC1 D8) by intersecting the provider's `openinspect_session_id` labels
with the session IDs read from the **CR-owned D1 `sessions` table**. This is
not a migration fallback; unmodified upstream cannot label sandboxes with
the CR UID, so the D1 intersection is the design.

The cleanup order must preserve ownership evidence:

1. Delete the Cloudflare Workers (stops new sessions and new sandboxes).
2. Read session IDs from the CR-owned D1 database.
3. List and terminate owned provider sandboxes.
4. Delete provider repository/environment images and the managed base
   artifact; stop the deployed provider app when no live CR still uses it.
5. Delete KV, R2, and D1 (D1 last — it is the ownership evidence).
6. Remove the Kubernetes finalizer only after confirmed cleanup.

If ownership cannot be proven, fail closed. Return an error, emit a warning
Event, retain the finalizer, and require an explicit `Orphan` policy or an
operator-approved recovery procedure. A network failure must never be
interpreted as successful deletion.

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
- make the managed base artifact the default: an **empty** artifact
  reference means the operator builds and manages it, an explicit reference
  means it is externally owned and used as-is (EPIC1 section 4). Never make
  a manually prebuilt artifact mandatory;
- provider app endpoints belong in **status**, discovered by the operator —
  never in spec.

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

Add table-driven tests to `internal/controller/sandbox_test.go`.

### 4. Watch every referenced Secret

Add all provider `SecretKeyRef` fields to `referencesSecret` in
`internal/controller/watches.go`, with tests in
`internal/controller/watches_test.go`. Never grant cross-namespace Secret
access.

### 5. Implement the provider artifact adapter

Implement the shared artifact-and-finalization lifecycle from EPIC1
section 5 under:

```text
internal/sandbox/provider/<provider>/
```

```go
type Provider interface {
	EnsureBaseArtifact(context.Context, RuntimeBundle) (Artifact, error)
	InspectBaseArtifact(context.Context, Artifact) (ArtifactState, error)
	DeleteBaseArtifact(context.Context, Artifact) error

	// Finalization only (EPIC1 D4). Ownership per D8.
	ListOwnedSandboxes(context.Context, OwnerMetadata) ([]Sandbox, error)
	TerminateSandbox(context.Context, string) error
}
```

Keep provider-specific API types inside the adapter; the controller
consumes normalized types and must not depend on raw SDK responses. There
is deliberately no session-sandbox lifecycle in this interface.

### 6. Run builds asynchronously

Remote artifact builds must not block reconciliation. The controller
creates owned Kubernetes Jobs named from the runtime digest
(`internal/controller/runtimebuild.go`, `cmd/runtime-builder`):

- the runtime-build Job builds the deterministically named artifact via the
  Go SDK / typed HTTP and exits successfully only after the remote artifact
  is usable;
- the modal-deploy Job (Modal only, D5) deploys the unmodified pinned
  upstream Modal app with the pinned Modal CLI; the controller then
  health-checks the endpoints and records them in status;
- rollout follows EPIC1 D6: build the new digest-named artifact, flip the
  reference, retain the previous artifact for a grace period, then delete
  it. No blue/green, no smoke-test sandboxes.

### 7. Wire the control-plane injection

Add the provider's plain variables and Secret bindings to
`sandboxInjection` in `internal/controller/sandbox.go`:

- `SANDBOX_PROVIDER` must equal the canonical provider enum value;
- variable names must match the pinned upstream control-plane contract;
- managed-artifact values (base snapshot names, discovered endpoints) come
  from **status**, never from spec;
- explicit Worker variables and bindings continue to win;
- Secret values remain Kubernetes Secret bindings and are never copied into
  the CR status;
- add injection and precedence tests to
  `internal/controller/sandbox_test.go`.

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

Managed artifact reconciliation publishes stable, provider-neutral status
(`status.sandboxRuntime`): provider, phase, artifact ID and name, runtime
digest and source revision, observed generation, last build time. Use
`metav1.Condition` for:

- `RuntimeReady` — the base artifact exists and matches the release digest;
- `ProviderAppReady` — the provider-side app is deployed and healthy
  (trivially `True` for providers without a deployed app).

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
- build/deploy Jobs are created, adopted, and reflected in status and
  conditions;
- deleting the CR invokes the provider cleanup hook;
- provider failure retains the finalizer;
- restoring credentials or provider health allows deletion to finish;
- `deletionPolicy: Orphan` skips provider calls;
- shared resources are not deleted while another CR uses them.

### Kind test with a mock provider

Add a mock provider API that supports artifact build, inspect, delete,
sandbox list/terminate, and injected failures. The Kind test must run in
the isolated Kind cluster required by `AGENTS.md`.

### Guarded live conformance test

A cost-limited live test proves the installer contract (EPIC1 section 9):

1. Apply one CR.
2. Wait for `RuntimeReady` and `ProviderAppReady`.
3. Create a session through the upstream control plane.
4. Verify a sandbox starts and the runtime bridge connects.
5. Delete the CR.
6. Query the provider and confirm no owned resources remain.

Live tests must use a dedicated account/project, strict timeout, sandbox
auto-expiration, and explicit cost limits.

## Pull request sequence

For a provider larger than a small SDK addition, use this sequence:

1. Capability matrix, typed client, normalized error model, and unit tests.
2. CRD configuration, validation, Secret watches, and regenerated manifests.
3. Base-artifact adapter (ensure/inspect/delete) with the runtime-build Job
   and status conditions.
4. Ownership metadata and finalizer cleanup.
5. Mock-provider Kind conformance test.
6. Sample CR, documentation, Helm synchronization, and guarded live test.

Do not merge a CRD enum value in isolation. Once users can select a
provider, the corresponding artifact lifecycle and cleanup must already be
safe.

## Definition of done

A provider is supported only when all of the following are true:

- One CR and Kubernetes Secret references are sufficient to configure it.
- No upstream patch, Terraform, CLI (outside the D5 modal-deploy Job), or
  manual provider deployment is needed.
- The operator builds or adopts its base artifact through code, and the
  pinned upstream control plane drives its session sandboxes.
- Every cleanup decision is backed by CR-owned ownership evidence (D8).
- Reconciliation is idempotent after process restarts and partial failures.
- Unknown provider state retains the finalizer.
- `Orphan` is explicit and does not occur accidentally because credentials
  are missing.
- Unit, envtest, Kind mock, and guarded live conformance tests pass.
- Generated CRDs, RBAC, Helm chart, samples, and documentation are current.
- `make lint` and `make test` pass.

## Final checklist

```text
[ ] Official SDK or typed HTTP client; no CLI/Terraform/subprocess (D5 Job excepted)
[ ] No upstream changes or private upstream endpoints
[ ] CRD provider spec, enum, markers, defaults, and status
[ ] Exactly-one-provider validation
[ ] Secret watch mapping
[ ] Base-artifact ensure/inspect/delete via adapter + runtime-build Job
[ ] Managed artifact default, explicit reference as external override
[ ] Ownership evidence from the CR-owned D1 sessions table (D8)
[ ] Idempotent, ownership-safe finalizer cleanup
[ ] Unknown health retains finalizer
[ ] Orphan policy tested
[ ] RuntimeReady / ProviderAppReady conditions
[ ] Unit, envtest, Kind mock, and live conformance coverage
[ ] Sample CR and Secret documentation
[ ] make manifests generate
[ ] make helm-sync
[ ] make lint
[ ] make test
```
