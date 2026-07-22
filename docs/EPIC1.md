# EPIC 1: Operator-Managed Provider Artifacts and Installation Lifecycle

## Goal

Applying one `OpenInspect` custom resource must install everything an
unmodified upstream [ColeMurray/background-agents](https://github.com/ColeMurray/background-agents)
("Open-Inspect") deployment needs: Cloudflare resources, worker bundles, D1
migrations, the web application, provider base artifacts, and the upstream
Modal application. The completed setup must not require Terraform, manually
run provider CLIs, manually supplied snapshot names, or manually created
ConfigMaps.

The operator is an **installer and lifecycle manager for unmodified
upstream**. It is not a runtime component: upstream's control plane drives
every running sandbox, and the operator never sits in the session path.

## Architecture decisions

These were settled during planning review and constrain everything below:

- **D1 — Installer, not middleman.** No operator-owned sandbox API. The
  previously planned per-CR `sandbox-api` Deployment, its HMAC endpoint
  design, and the Ingress/Gateway endpoint modes are removed. Upstream's
  control plane makes all provider calls at runtime, as designed upstream.
- **D2 — Upstream is never modified.** No forks, no patches, no source
  changes — including the control-plane worker and the web application. The
  web image keeps its build-time provider configuration; no
  `/api/platform-info` change.
- **D3 — Supported providers are derived from the pinned upstream
  revision**, not listed in this document. Today that is Modal and Daytona
  (see [ADDING_SANDBOX_PROVIDER.md](./ADDING_SANDBOX_PROVIDER.md)). Vercel
  and OpenComputer join when the pinned upstream revision actually drives
  them; their CRD scaffolding must not be advertised as supported until then.
- **D4 — The operator owns base artifacts and cleanup only.** It builds,
  inspects, and deletes provider base artifacts, and terminates owned
  sandboxes during finalization. It never creates, snapshots, resumes, or
  stops sandboxes for running sessions.
- **D5 — The upstream Modal app is deployed by an operator-owned Job**
  running the pinned Modal CLI against the pinned upstream source. Modal
  Functions cannot be deployed through the Go SDK (deployment requires
  Modal's Python toolchain); this Job is the single scoped exception to the
  no-CLI rule in ADDING_SANDBOX_PROVIDER.md, which must be amended
  accordingly. Everything else remains Go SDK or typed HTTP only.
- **D6 — No blue/green.** Artifacts are digest-named; the operator builds
  the new artifact, flips the reference, retains the previous artifact for a
  grace period, then garbage-collects it. No smoke-test sandboxes.
- **D7 — No migration machinery.** There are zero existing installations.
  No `managementPolicy: External` compatibility mode, no deprecation cycle.
- **D8 — D1 session-intersection is the permanent ownership mechanism.**
  Unmodified upstream cannot label sandboxes with the CR UID, so ownership
  is always established by intersecting provider sandbox labels with the
  CR-owned D1 `sessions` table. It is not a migration fallback.

## Target architecture

```text
OpenInspect CR
    |
    +-- OpenInspect controller
         +-- Cloudflare resources (D1, KV, R2, Workers)
         +-- embedded worker bundles + D1 migrations
         +-- web Deployment + Service
         +-- runtime-build Job  -> provider base artifact (Go SDK / HTTP)
         +-- modal-deploy Job   -> upstream Modal app (pinned Modal CLI)

Runtime (operator not involved):
Cloudflare control-plane worker
    +-- Modal path:   HTTPS -> upstream Modal app -> Modal sandboxes
    +-- Daytona path: HTTPS -> Daytona REST API   -> Daytona sandboxes
```

References:

- [Modal JavaScript and Go SDKs](https://modal.com/docs/guide/sdk-javascript-go)
- [Modal sandbox snapshots](https://modal.com/docs/guide/sandbox-snapshots)
- [Daytona Go SDK](https://www.daytona.io/docs/en/go-sdk/daytona/)

## Scope and ownership

The operator owns:

- the pinned upstream Open-Inspect source revision;
- the platform worker bundles and D1 migrations;
- the web application image;
- the provider-neutral runtime manifest and its base artifacts (images,
  snapshots);
- deployment of the upstream Modal application;
- provider artifact garbage collection;
- finalization of all externally owned resources.

The only external prerequisites are:

- provider accounts and API credentials;
- OAuth and external application registrations that have no management API.

## Implementation plan

### 1. Import and pin the upstream runtime

Add a curated upstream source area:

```text
third_party/background-agents/
  UPSTREAM_COMMIT
  LICENSE
  packages/sandbox-runtime/
  packages/web/
  packages/control-plane/
  packages/shared/
  <modal app source directory>
  terraform/d1/migrations/
```

The synced file list must be audited against what each supported provider's
installation actually consumes — in particular the Modal application source,
which the deploy Job in section 6 requires.

Add:

- `hack/sync-upstream.sh <commit>`;
- checksum verification;
- upstream MIT attribution;
- a CI check that generated runtime artifacts match the pinned commit;
- Renovate or a scheduled pull request for upstream updates.

The running operator must never fetch upstream `main`. Every operator release
pins one exact upstream commit.

### 2. Create a canonical runtime bundle

Move the duplicated provider image recipe into a provider-neutral package:

```text
internal/runtime/
  manifest.go
  files.go
  versions.go
  digest.go
  testdata/
```

It must contain:

- pinned Node, Python, OpenCode, agent-browser, code-server, and ttyd versions;
- system packages;
- the sandbox entrypoint and bridge;
- tools and plugins;
- embedded `sandbox-runtime` files;
- a deterministic SHA-256 digest.

Every provider adapter consumes the same manifest, so a tool or runtime
change produces one new digest for all providers.

### 3. Publish release-coupled images

```text
ghcr.io/dirien/openinspect-operator:<version>
ghcr.io/dirien/openinspect-sandbox-runtime:<version>
```

All images carry the operator version, the upstream commit, the runtime
digest, OCI source and revision labels, an SBOM, and a cosign signature.

The sandbox runtime image is the common base for provider artifacts. Web
images retain their current per-provider, per-deployment builds because
`NEXT_PUBLIC_*` values are baked in at build time (D2 forbids the upstream
change that would lift this).

### 4. CRD: managed artifacts by default, references as overrides

```yaml
spec:
  sandbox:
    provider: modal
    runtime:
      image: ""               # Empty means the release's bundled runtime.
      updatePolicy: OnUpgrade
      deletionPolicy: Delete
```

- An empty artifact reference means the operator builds and manages the
  artifact. A set reference (Daytona `baseSnapshot`, Vercel snapshot/image,
  OpenComputer `template`) means it is externally owned and used as-is.
- Modal deployed-app endpoint fields move from spec to status: the operator
  deploys the app and discovers the endpoint itself.
- No `managementPolicy` field (D7): the presence of an explicit reference is
  the override.

Add runtime status:

```yaml
status:
  sandboxRuntime:
    provider: daytona
    phase: Ready
    sourceRevision: ed34794...
    digest: sha256:...
    artifactName: oi-<uid>-<digest>
    artifactID: ...
    observedGeneration: 4
    lastBuildTime: ...
```

Use `metav1.Condition` for:

- `RuntimeReady` — the base artifact exists and matches the release digest;
- `ProviderAppReady` — the upstream Modal app is deployed and healthy
  (trivially `True` for providers without a deployed app).

After changing API types or markers, run `make manifests` and
`make generate` (and `make helm-sync`).

### 5. Implement provider artifact adapters

```go
type Provider interface {
	EnsureBaseArtifact(context.Context, RuntimeBundle) (Artifact, error)
	InspectBaseArtifact(context.Context, Artifact) (ArtifactState, error)
	DeleteBaseArtifact(context.Context, Artifact) error

	// Finalization only (D4). Ownership per D8.
	ListOwnedSandboxes(context.Context, OwnerMetadata) ([]Sandbox, error)
	TerminateSandbox(context.Context, string) error
}
```

```text
internal/sandbox/provider/
  provider.go
  modal/
  daytona/
```

- **Modal:** official `modal-client/go` SDK. Create or find the app record,
  build or pull the base image, inspect and delete artifacts, list and
  terminate sandboxes during finalization. Never deploy Modal Functions from
  Go — that is the deploy Job's task (section 6).
- **Daytona:** official Go SDK declarative image and snapshot APIs, which
  support build contexts and streamed snapshot builds directly.

There is no sandbox runtime lifecycle in this interface. Session sandboxes
are created, snapshotted, resumed, and stopped by upstream's control plane.

### 6. Asynchronous builds and the Modal deploy Job

Remote builds must not block reconciliation.

```text
cmd/runtime-builder/main.go
internal/controller/runtimebuild.go
```

The controller creates owned Kubernetes Jobs named from the runtime digest:

- **runtime-build Job** (all providers): Go SDK / HTTP only, mounts only the
  selected provider Secret, builds a deterministically named artifact, exits
  successfully only after the remote artifact is usable.
- **modal-deploy Job** (Modal only, D5): a builder image containing the
  pinned Modal CLI deploys the unmodified pinned upstream Modal app into the
  tenant's Modal environment. Idempotent; re-runs on upstream bump or
  runtime digest change. On success the controller health-checks the
  deployed endpoints, records the app URL in status, and injects it into the
  control-plane worker configuration.

Rollout (D6): build the new digest-named artifact, flip the reference the
control plane consumes, retain the previous artifact for a rollback grace
period, then delete it.

### 7. Package worker bundles and migrations

Users must not create `worker-bundles` or `d1-migrations` ConfigMaps.

Build bundles during the operator release and embed them:

```text
internal/platformbundle/
  control-plane.js.gz
  github-bot.js.gz
  slack-bot.js.gz
  linear-bot.js.gz
  migrations/
  metadata.json
```

Use `go:embed` and apply built-in bundles by default. Retain ConfigMap and
URL overrides only for development. The platform bundle version, web image,
runtime image, and operator version advance atomically.

### 8. Cleanup and ownership

The upstream-compatible cleanup baseline is the permanent design (D8):

- never add or depend on `api-stop-app`, `api-terminate-sandboxes`, or any
  other private Modal web function;
- use Modal's Go SDK to find the `open-inspect` app in the CR's environment,
  list and terminate both v1 and v2 sandboxes, and stop the app through
  Modal's authenticated provider API;
- use the Daytona Go SDK to list sandboxes carrying
  `openinspect_framework=open-inspect` and delete them through
  `DELETE /sandbox/{id}`;
- never interpret `openinspect_framework` alone as ownership — every
  Open-Inspect deployment uses that value;
- read session IDs from the CR-owned D1 `sessions` table and delete a
  sandbox only when its `openinspect_session_id` label occurs in that set;
- query D1 and finish provider cleanup before deleting D1 itself;
- keep the finalizer on missing credentials, provider errors, D1 ownership
  query failures, timeouts, DNS failures, or unknown provider health.

Finalization order:

1. Terminate every owned provider sandbox (D8 ownership evidence).
2. Delete provider repository and environment images.
3. Delete the managed base artifact.
4. Stop the deployed upstream Modal app.
5. Remove Cloudflare resources.
6. Remove the finalizer only after confirmed cleanup.

A network failure must never be interpreted as successful deletion.
`deletionPolicy: Orphan` retains remote artifacts and writes their complete
references into status and events before finalizer removal.

### 9. Testing gates

- **Unit:** provider clients behind interfaces, retry and error
  classification, manifest and digest stability, ownership logic.
- **Envtest:** build and deploy Jobs, status transitions, finalizers, Secret
  watches.
- **Kind:** mock provider API implementing artifact builds and cleanup.
- **Live nightly:** one guarded, cost-limited test per supported provider
  (currently Modal and Daytona).

Each live conformance test proves the installer contract:

1. Apply one CR.
2. Wait for `RuntimeReady` and `ProviderAppReady`.
3. Create a session through the upstream control plane.
4. Verify a sandbox starts and the runtime bridge connects.
5. Delete the CR.
6. Confirm no owned provider resources remain.

## Pull request sequence

1. Pin and synchronize upstream (including the Modal app source); create the
   canonical runtime manifest.
2. Build the runtime release image.
3. Add CRD managed-runtime fields and status; amend
   ADDING_SANDBOX_PROVIDER.md with the D5 exception.
4. Add the provider artifact interface and the Daytona implementation.
5. Add the Modal implementation and the modal-deploy Job; remove manual
   Modal app deployment from the documented install path.
6. Embed worker bundles and migrations.
7. Add artifact garbage collection and finalization hardening.
8. Add live provider conformance tests.

## Future work (out of scope)

- **Vercel and OpenComputer adapters** — blocked on the pinned upstream
  revision driving these providers (D3). When unblocked, note that Vercel
  custom images are public beta and the OpenComputer template API is alpha;
  entering this work accepts that API risk.
- Lifting the per-provider web image builds, which requires an upstream
  change (D2).

## Definition of done

The epic is complete when installation requires only provider credentials
and one custom resource:

```sh
kubectl create secret ...provider credentials...
kubectl apply -f openinspect.yaml
```

No human must:

- run Terraform;
- run `modal deploy` or any provider CLI;
- build a provider snapshot manually;
- create worker or migration ConfigMaps;
- copy an artifact ID or app endpoint into the CR.

The only remaining prerequisites are provider accounts and credentials and
OAuth application registrations.
