# EPIC 1: Operator-Managed Sandbox Runtime and Lifecycle

## Goal

Applying one `OpenInspect` custom resource must cause the operator to build or
reconcile the runtime artifacts and operate every sandbox through provider
APIs. The completed setup must not require Terraform, provider CLIs, manually
supplied snapshot names, or a manually deployed Modal application.

Cloudflare Workers initiate sessions while the operator runs inside
Kubernetes. To let the operator own each sandbox without exposing the manager
and its Kubernetes privileges, the operator will reconcile a separate,
externally reachable `sandbox-api` workload for each `OpenInspect` resource.
This workload remains part of this repository and the operator release.

## Target architecture

```text
OpenInspect CR
    |
    +-- OpenInspect controller
    |    +-- Cloudflare resources
    |    +-- provider base artifact
    |    +-- web Deployment
    |    +-- per-CR sandbox-api Deployment
    |    +-- external sandbox endpoint
    |
Cloudflare control-plane
    |
    | authenticated HTTPS
    v
sandbox-api
    +-- Modal Go SDK
    +-- Daytona Go SDK
    +-- Vercel REST API
    +-- OpenComputer HTTP API
         |
         v
   provider sandboxes
```

Each `OpenInspect` instance receives its own `sandbox-api` Deployment. It is
given only that tenant's provider credentials and has no Kubernetes RBAC.

Modal's Go SDK can create sandboxes, build images, and take filesystem
snapshots directly. It cannot deploy Modal Functions, but the new architecture
does not require Modal Functions because `sandbox-api` replaces the upstream
Modal HTTP application.

References:

- [Modal JavaScript and Go SDKs](https://modal.com/docs/guide/sdk-javascript-go)
- [Modal sandbox snapshots](https://modal.com/docs/guide/sandbox-snapshots)
- [Daytona Go SDK](https://www.daytona.io/docs/en/go-sdk/daytona/)
- [Vercel REST API](https://vercel.com/docs/rest-api)
- [Vercel custom sandbox images](https://vercel.com/changelog/vercel-sandbox-now-support-custom-images)
- [OpenComputer templates](https://docs.opencomputer.dev/sandboxes/templates)

## Scope and ownership

The operator owns:

- the pinned upstream Open-Inspect source revision;
- the platform worker bundles and D1 migrations;
- the web application image;
- the provider-neutral sandbox runtime;
- provider base images, snapshots, or templates;
- the per-tenant sandbox API;
- creation, restoration, snapshotting, resumption, and termination of session
  sandboxes;
- prebuilt repository and environment images;
- provider artifact garbage collection;
- finalization of all externally owned resources.

The only external prerequisites are:

- provider accounts and API credentials;
- OAuth and external application registrations that have no management API;
- an externally reachable HTTPS route for `sandbox-api`, implemented by the
  user's chosen Ingress, Gateway, tunnel, or proxy system.

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
  terraform/d1/migrations/
```

Add:

- `hack/sync-upstream.sh <commit>`;
- checksum verification;
- upstream MIT attribution;
- a CI check that generated runtime artifacts match the pinned commit;
- Renovate or a scheduled pull request for upstream updates.

The running operator must never fetch upstream `main`. Every operator release
must pin one exact upstream commit.

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

Every provider adapter must consume the same manifest. A tool or runtime
change must therefore produce one new digest for all providers.

### 3. Publish release-coupled images

Build and publish these artifacts from this repository:

```text
ghcr.io/dirien/openinspect-operator:<version>
ghcr.io/dirien/openinspect-sandbox-api:<version>
ghcr.io/dirien/openinspect-sandbox-runtime:<version>
ghcr.io/dirien/openinspect-web:<version>
```

All images must carry:

- the operator version;
- the upstream commit;
- the runtime digest;
- OCI source and revision labels;
- an SBOM;
- a cosign signature.

The sandbox runtime image is the common base for Modal and Daytona. Vercel can
consume a custom OCI sandbox image from Vercel Container Registry. Because
this feature is currently public beta, its provider adapter must retain a
snapshot-based fallback.

OpenComputer must receive a provider-specific declarative image JSON generated
from the same canonical runtime manifest.

### 4. Change the CRD from artifact references to managed artifacts

Add common runtime configuration:

```yaml
spec:
  sandbox:
    provider: modal
    managementPolicy: Managed
    runtime:
      image: ""               # Empty means the release's bundled runtime.
      updatePolicy: OnUpgrade
      deletionPolicy: Delete
```

Provider configuration must contain credentials and provider options rather
than mandatory prebuilt artifact names.

API changes:

- make Daytona `baseSnapshot` an optional external override;
- make OpenComputer `template` an optional external override;
- retain Vercel snapshot/image references as optional overrides;
- remove Modal `apiSecretSecretRef`, environment web suffix, and deployed-app
  endpoint fields from the managed path;
- temporarily retain `managementPolicy: External` for migration compatibility.

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

- `RuntimeReady`;
- `SandboxAPIReady`;
- `SandboxEndpointReady`.

After changing API types or markers, run `make manifests` and `make generate`.

### 5. Implement provider adapters

Create a shared interface:

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

Implement it under:

```text
internal/sandbox/provider/
  provider.go
  modal/
  daytona/
  vercel/
  opencomputer/
```

Provider implementations:

- **Modal:** Use the official `modal-client/go` SDK. Create or find the app
  record, build or pull the base image, create sandboxes, execute the runtime
  entrypoint, snapshot filesystems, restore, and terminate. Do not deploy
  Modal Functions and do not invoke `modal deploy`.
- **Daytona:** Use the official Go SDK's declarative image and snapshot APIs.
  It supports build contexts and streamed snapshot builds directly.
- **Vercel:** Implement a typed Go REST client for sandbox creation, commands,
  files, snapshots, stop, and delete. Prefer the release runtime OCI image and
  retain the snapshot builder as a fallback while custom images are beta.
- **OpenComputer:** Implement a typed Go HTTP client for declarative image
  manifests, snapshots, and sandbox lifecycle. Keep this adapter isolated
  because the template API is alpha.

### 6. Make base-artifact builds asynchronous

Remote builds must not block reconciliation for several minutes.

Add:

```text
cmd/runtime-builder/main.go
internal/controller/runtimebuild.go
```

The controller creates an owned Kubernetes Job named from the runtime digest.
The Job:

- contains no provider CLI binaries;
- invokes only Go SDK or HTTP code;
- mounts only the selected provider Secret;
- builds a deterministic artifact name;
- exits successfully only after the remote artifact is usable.

The controller then inspects the artifact through the provider API and updates
status.

Runtime rollout must be blue/green:

1. Build the new artifact.
2. Start a smoke-test sandbox.
3. Verify runtime health and bridge startup.
4. Switch `sandbox-api` to the new artifact.
5. Retain the previous artifact for a rollback window.
6. Delete the previous artifact after the grace period.

### 7. Add the operator-owned sandbox API

Add:

```text
cmd/sandbox-api/main.go
internal/sandboxapi/
  server.go
  auth.go
  request.go
  response.go
  metrics.go
```

Expose a versioned API:

```text
GET    /v1/health
POST   /v1/sandboxes
POST   /v1/sandboxes/restore
POST   /v1/sandboxes/{id}/snapshot
POST   /v1/sandboxes/{id}/resume
POST   /v1/sandboxes/{id}/stop
POST   /v1/images/build
DELETE /v1/images/{id}
```

Initially reuse the upstream Modal endpoint request and response shapes. They
already represent create, restore, snapshot, and image-build operations, which
minimizes the control-plane change.

Every request must include:

- deployment UID;
- session and sandbox IDs;
- timestamp and nonce;
- HMAC signature;
- correlation and trace IDs.

Every remote provider resource must carry ownership metadata:

```text
openinspect.uid
openinspect.namespace
openinspect.name
openinspect.session_id
openinspect.sandbox_id
openinspect.runtime_digest
```

### 8. Reconcile one sandbox-api Deployment per CR

The operator creates:

- a Deployment;
- a Service;
- provider Secret mounts;
- a NetworkPolicy;
- a PodDisruptionBudget when replicas exceed one;
- readiness and liveness probes;
- immutable runtime-artifact configuration.

The pod must:

- run non-root;
- use a read-only root filesystem;
- have no Kubernetes API token;
- receive only the selected provider credentials;
- never log request environment variables or tokens.

Provider credentials must be removed from the Cloudflare control-plane worker.
The worker receives only the sandbox API URL and HMAC secret.

### 9. Provide pluggable external connectivity

The Cloudflare control-plane must be able to reach `sandbox-api`, but the
operator must not require a particular ingress vendor. It must always create a
stable namespaced `ClusterIP` Service:

```text
Service: <openinspect-name>-sandbox-api
Port:    8080
Paths:   /v1/*
```

Users can expose this Service through nginx, ngrok, Traefik, HAProxy, an
existing cloud load balancer, Gateway API, or another system. The application
contract is only an externally reachable HTTPS URL; routing implementation is
not part of the sandbox provider contract.

Add an endpoint configuration with three modes:

```yaml
spec:
  sandbox:
    endpoint:
      mode: Ingress
      publicURL: https://sandboxes.example.com
      ingress:
        className: nginx # nginx, ngrok, traefik, or another IngressClass
        annotations: {}
        host: sandboxes.example.com
        tlsSecretName: sandboxes-example-com-tls
```

In `Ingress` mode, the operator creates a standard
`networking.k8s.io/v1/Ingress`. It must not depend on vendor CRDs. The selected
Ingress controller interprets `className` and optional annotations. The
operator generates the backend Service reference and restricts the route to
`/v1`.

The generated resource is equivalent to:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: example-sandbox-api
  namespace: example
spec:
  ingressClassName: nginx # May also be ngrok, traefik, etc.
  tls:
    - hosts: [sandboxes.example.com]
      secretName: sandboxes-example-com-tls
  rules:
    - host: sandboxes.example.com
      http:
        paths:
          - path: /v1
            pathType: Prefix
            backend:
              service:
                name: example-sandbox-api
                port:
                  number: 8080
```

Support an externally managed route without creating an Ingress:

```yaml
spec:
  sandbox:
    endpoint:
      mode: External
      publicURL: https://sandbox-api.example.com
```

In `External` mode, the operator creates only the Service. A user, platform
operator, Helm release, ngrok integration, or other automation owns the route.
The operator validates the URL, waits for its health endpoint, and injects it
into the Cloudflare control-plane worker.

Gateway API can be supported as an additional convenience mode without making
it mandatory:

```yaml
spec:
  sandbox:
    endpoint:
      mode: GatewayAPI
      publicURL: https://sandboxes.example.com
      gatewayAPI:
        parentRef:
          name: public
          namespace: gateway-system
        hostname: sandboxes.example.com
```

In `GatewayAPI` mode, the operator creates an `HTTPRoute` targeting the same
ClusterIP Service. This mode must be optional so clusters without Gateway API
can use standard Ingress or an externally managed route.

Endpoint reconciliation rules:

1. Reconcile the `sandbox-api` Deployment and ClusterIP Service first.
2. Create an Ingress or HTTPRoute only when its corresponding mode is selected.
3. Resolve the effective HTTPS URL from `publicURL` or the route status.
4. Check `GET <publicURL>/v1/health` before publishing the URL to the worker.
5. Set `SandboxEndpointReady=True` only after the external health check passes.
6. Requeue without rolling out a broken URL when DNS, TLS, or routing is not
   ready.
7. On mode changes, make the new route healthy before deleting the old route.

Security requirements apply regardless of ingress implementation:

- production endpoints require HTTPS;
- HMAC authentication and nonce replay protection remain mandatory even when
  the ingress adds its own authentication;
- only `/v1/*` is routed to `sandbox-api`;
- request body limits, timeouts, and rate limits have safe defaults that users
  can override with ingress annotations where required;
- the manager's metrics, health, webhook, and Kubernetes credentials are never
  exposed;
- `sandbox-api` continues to run without a Kubernetes service-account token.

Add ready-to-copy examples under `config/samples/exposure/` for generic
Ingress, nginx, ngrok, Traefik, Gateway API, and externally managed routes.

### 10. Change the upstream control-plane integration

Build the control-plane bundle from the pinned upstream source with one
architectural change: replace provider-specific clients with a generic
`SandboxAPIClient`.

The operator injects:

```text
SANDBOX_PROVIDER
SANDBOX_API_URL
SANDBOX_API_SECRET
```

Remove these provider credentials from the Cloudflare worker:

```text
MODAL_TOKEN_*
DAYTONA_API_KEY
VERCEL_TOKEN
OPENCOMPUTER_API_KEY
```

The control plane retains session decisions and D1 state. The Kubernetes-side
sandbox API owns every provider call.

### 11. Package worker bundles and migrations

Users must not have to create `worker-bundles` or `d1-migrations` ConfigMaps.

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

Use `go:embed` and apply built-in bundles by default. Retain ConfigMap and URL
overrides only for development.

The platform bundle version, web image, runtime image, sandbox API image, and
operator version must advance atomically.

### 12. Make the web image provider-neutral

Add a production `Dockerfile.web` for the upstream standalone Next.js output.

Remove separate `:modal` and `:daytona` images. The web app must obtain
provider capabilities at runtime from `/api/platform-info`, rather than from a
build-time `NEXT_PUBLIC_SANDBOX_PROVIDER` value.

The operator automatically injects:

```text
SANDBOX_PROVIDER
CONTROL_PLANE_URL
NEXTAUTH_URL
```

This produces one immutable web image for all providers.

### 13. Implement cleanup and ownership

#### Current upstream-compatible finalization baseline

The existing operator must be safe before the new `sandbox-api` architecture
lands. Its cleanup implementation must work with an unmodified checkout of
`ColeMurray/background-agents`:

- never add or depend on `api-stop-app`, `api-terminate-sandboxes`, or any
  other private Modal web function;
- use Modal's Go SDK to find the `open-inspect` app in the CR's environment,
  list and terminate both v1 and v2 sandboxes, and stop the app with Modal's
  authenticated provider API;
- use the Daytona Go SDK to list sandboxes carrying
  `openinspect_framework=open-inspect` and delete them through
  `DELETE /sandbox/{id}`;
- never interpret `openinspect_framework` alone as ownership because every
  Open-Inspect deployment uses that value;
- read session IDs from the CR-owned D1 `sessions` table and delete a Daytona
  sandbox only when its `openinspect_session_id` label occurs in that set;
- query D1 and finish provider cleanup before deleting D1 itself;
- keep the finalizer on missing credentials, provider errors, D1 ownership
  query failures, timeouts, DNS failures, or unknown Modal health.

This baseline is an operator-only change. It does not modify, fork, or patch
upstream source. After the managed `sandbox-api` path is complete, CR UID
labels become the primary ownership key and the D1 intersection remains a
migration fallback for sandboxes created by older releases.

Finalization must:

1. Stop or terminate every provider sandbox tagged with the CR UID.
2. Delete provider repository and environment images.
3. Delete the managed base artifact.
4. Remove operator-created endpoint resources.
5. Remove Cloudflare resources.
6. Remove the finalizer only after confirmed cleanup.

Unknown provider health must retain the finalizer and retry. A network failure
must never be interpreted as successful deletion.

`deletionPolicy: Orphan` must retain remote artifacts and write their complete
references into status and events before finalizer removal.

### 14. Add testing gates

Add four test levels:

- **Unit:** Provider clients behind interfaces, retry and error
  classification, manifests, ownership, and digest stability.
- **Envtest:** Runtime-build Job, Deployment, status transitions, finalizers,
  and Secret watches.
- **Kind:** Mock provider API implementing builds, sandboxes, snapshots,
  restore, and cleanup.
- **Live nightly:** One guarded, cost-limited provider test for each provider.

Every live conformance test must prove:

1. Apply one CR.
2. Wait for the base runtime to become ready.
3. Create a sandbox.
4. Verify the runtime bridge connects.
5. Execute a command.
6. Snapshot the sandbox.
7. Restore it.
8. Stop it.
9. Delete the CR.
10. Confirm no owned provider resources remain.

## Recommended pull request sequence

1. Pin and synchronize upstream; create the canonical runtime manifest.
2. Build the web, runtime, and sandbox API release images.
3. Add CRD managed-runtime fields and status.
4. Add the provider interface and Daytona implementation.
5. Add the Vercel implementation.
6. Add the OpenComputer implementation.
7. Add the direct Modal Go implementation and remove Modal app deployment.
8. Add the `sandbox-api` Deployment, stable Service, generic Ingress, external
   URL, and optional Gateway API support.
9. Switch the control-plane bundle to `SandboxAPIClient`.
10. Embed worker bundles and migrations.
11. Add blue/green artifact upgrades and cleanup.
12. Add live provider conformance tests.
13. Remove external-artifact compatibility fields after one deprecation cycle.

## Definition of done

The epic is complete when installation requires only provider credentials and
one custom resource:

```sh
kubectl create secret ...provider credentials...
kubectl apply -f openinspect.yaml
```

No human must:

- run Terraform;
- run `modal deploy`;
- run Daytona, Vercel, or OpenComputer CLIs;
- build a provider snapshot manually;
- create worker or migration ConfigMaps;
- choose a provider-specific web image;
- copy an artifact ID into the CR.

The only remaining prerequisites are provider accounts and credentials, OAuth
application registrations, and an externally reachable HTTPS route implemented
with the user's preferred ingress, gateway, tunnel, or proxy system.
