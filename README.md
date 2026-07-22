# openinspect-operator

A Kubernetes operator that stands up the entire [Open-Inspect / background-agents](https://github.com/ColeMurray/background-agents)
platform from a single custom resource — pure Go and the Cloudflare SDK, no
Terraform, no shell scripts.

One `OpenInspect` CR replaces what previously took a two-phase `terraform
apply`, 13 `null_resource` script shims, `scripts/d1-migrate.sh`, and
`scripts/wrangler-secrets.sh`:

| Concern | How the operator handles it |
|---|---|
| D1 database | get-or-create by name via the D1 API |
| D1 schema migrations | numbered `.sql` keys in a ConfigMap, applied in order via the D1 query API, tracked in `_schema_migrations` (same contract as upstream `d1-migrate.sh`) |
| KV namespaces / R2 buckets | get-or-create by name |
| Workers | classic multipart script upload with all bindings (D1, KV, R2, Durable Objects, service, plain-text vars, secrets), cron triggers, workers.dev subdomain |
| Worker secrets | read from Kubernetes Secrets, uploaded as `secret_text` bindings |
| Service bindings between workers | converge across reconcile passes — a binding to a not-yet-existing worker is retried until the target exists, so circular references (control-plane ↔ bots) need no two-phase apply |
| Durable Object migrations | `new_sqlite_classes` declared automatically on first upload of a worker |
| Web UI | runs **in-cluster** as an owned Deployment + Service (env from spec, secrets via `secretKeyRef`) |
| Worker bundles & D1 migrations | **embedded in the operator release**, built from the pinned upstream revision — no `worker-bundles` or `d1-migrations` ConfigMaps to create (both remain as development overrides) |
| Sandbox providers | `spec.sandbox` selects **modal / daytona** (vercel / opencomputer scaffolding exists but is unsupported until the pinned upstream drives them); the operator injects that provider's vars and secret bindings into the control-plane worker (explicit worker values win) |
| Provider base artifacts | **operator-managed by default**: for Daytona a digest-named base snapshot is built by an owned runtime-build Job from the canonical runtime manifest (`internal/runtime`, pinned toolchain + embedded `sandbox_runtime`); `spec.sandbox.daytona.baseSnapshot` remains the external override. Superseded artifacts are retained for a rollback grace period, then garbage-collected |
| Modal app deployment | the operator deploys the **unmodified pinned upstream Modal app** with an owned modal-deploy Job (pinned Modal CLI + pinned source baked into `ghcr.io/dirien/openinspect-modal-deploy`), converges the three named Modal secrets it needs via Modal's API, discovers the deployed endpoints, health-checks them, and injects the web suffix into the control plane — `modal deploy` by hand is gone |
| Platform app secrets | `spec.appSecrets` seeds the platform's global secrets store (what sandboxes receive as env, e.g. `ANTHROPIC_API_KEY`) through the control-plane worker's own API — no web-UI clickops; removed keys are pruned, keys added via the UI are left alone, a wiped store re-seeds itself |
| Deletion | finalizer removes all Cloudflare resources, terminates the CR's Modal sandboxes and **stops its Modal app** via Modal's Go SDK, and deletes the Daytona sandboxes whose labels match this CR's D1 session index — other installations' sandboxes are never touched; `deletionPolicy: Orphan` keeps everything, including running provider compute |

Version uploads are gated by a content hash over script bytes, bindings,
vars, and secret values — unchanged workers never churn new versions.
Referenced ConfigMaps and Secrets are **watched** (metadata-only, read
uncached), so editing a bundle, migration, or rotated secret rolls out
immediately — no CR touch needed. Independently, the idempotent ensure pass
re-runs every 5 minutes as drift correction; tune it with the manager's
`--resync-interval` flag.

Removing a worker, KV namespace, or R2 bucket from the spec deletes it
remotely (tracked in status, so this survives spec edits; a non-empty R2
bucket is orphaned with a warning event instead of wedging the reconcile).
Removing `spec.web` tears down the web Deployment/Service. The D1 database is
only ever deleted together with the CR — migrations make it data-bearing.
Cross-field mistakes (bindings referencing undefined namespaces/buckets/
workers, duplicate worker names) are rejected as a terminal `InvalidSpec`
condition instead of erroring against the Cloudflare API.

Each CR is one complete installation, isolated per Modal *environment*
(`spec.sandbox.modal.environment` names it). The operator deploys the
upstream Modal app into that environment itself and discovers the app's
`*.modal.run` web suffix from the deployed endpoints into
`status.modalApp` — nothing endpoint-shaped is configured in the spec.
Teardown needs no CLI: on CR deletion the operator terminates the app's
sandboxes, deletes the recorded prebuild images (from the CR-owned D1
`image_builds` evidence), deletes the managed base artifacts, and stops the
app through Modal's Go SDK. Daytona cleanup is scoped by ownership
evidence: the operator reads the session IDs from the CR's own D1 database
and deletes only sandboxes carrying those session labels, so several
installations can share one Daytona organization safely.
`spec.sandbox.runtime.deletionPolicy: Orphan` retains managed artifacts and
records their references before the finalizer is removed.

The operator pins one exact upstream revision per release
(`third_party/background-agents/UPSTREAM_COMMIT`, synced and CI-verified by
`hack/sync-upstream.sh`); the runtime toolchain pins are parity-tested
against that revision, and worker bundles are rebuilt and byte-compared in
CI. The release also ships `ghcr.io/dirien/openinspect-sandbox-runtime`
(the provider-neutral runtime base image, usable as
`spec.sandbox.runtime.image`).

Out of scope (not automatable via APIs): GitHub/Slack/Linear/Google app
registrations and the per-deployment web image builds (`NEXT_PUBLIC_*`
values are baked in at build time by upstream's design).

## Custom resource

See [config/samples/openinspect_v1alpha1_openinspect.yaml](config/samples/openinspect_v1alpha1_openinspect.yaml)
for a full example. The essentials:

```yaml
apiVersion: openinspect.dirien.dev/v1alpha1
kind: OpenInspect
metadata:
  name: prod
spec:
  deploymentName: prod                       # suffix for all Cloudflare names, immutable
  cloudflare:
    accountId: "..."
    apiTokenSecretRef: {name: cloudflare-credentials, key: apiToken}
    # apiBaseURL: optional override (regional gateway or test mock)
  d1: {}                                     # embedded migrations apply by default
  kvNamespaces: [session-index]
  r2Buckets: [media]
  workers:
    - name: control-plane                    # embedded release bundle; script: is a dev override
      bindings:
        d1: [{binding: DB}]
        kv: [{binding: REPOS_CACHE, namespace: session-index}]
        r2: [{binding: MEDIA_BUCKET, bucket: media}]
        durableObjects: [{binding: SESSION, className: SessionDO}]
        services: [{binding: SLACK_BOT, worker: slack-bot}]
      vars: {APP_NAME: Open Inspect}
      secrets:
        - {binding: GITHUB_CLIENT_SECRET, secretRef: {name: platform-secrets, key: githubClientSecret}}
      cron: ["* * * * *"]
  appSecrets:                                # seeded into the platform's secrets store
    - {key: ANTHROPIC_API_KEY, secretRef: {name: platform-secrets, key: anthropicApiKey}}
  web:
    image: ghcr.io/your-org/open-inspect-web:latest
```

Cloudflare resource names expand to `<namePrefix>-<logical>-<deploymentName>`
(prefix defaults to `open-inspect`), matching the upstream Terraform naming.
Both parts are immutable.

`kubectl get oi` shows Ready state, reason, and applied migration count.

## Multi-tenancy

The operator is built for platform teams offering open-inspect as a
self-service on a shared cluster: many `OpenInspect` CRs, one operator.

- **Credentials are per-CR.** Every CR references its own API token Secret in
  its own namespace; there is no operator-level token. The recommended model
  is one Cloudflare account per tenant — blast radius, billing, and the API
  rate limit (per token) all shard along with it. A no-op reconcile pass
  costs roughly a dozen API calls, so even hundreds of CRs per token stay far
  below Cloudflare's 1200 requests/5 min limit.
- **Names cannot collide.** If a younger CR claims the same
  `accountId` + `namePrefix` + `deploymentName` as an existing one, it is
  refused with a `DeploymentNameConflict` condition before any Cloudflare
  call. The oldest claimant always wins, deterministically.
- **Existing remote resources are never captured silently.** A CR that finds
  a D1 database, KV namespace, R2 bucket, or worker script already existing
  under one of its names — created by another tool, another cluster, or a
  previously orphaned CR — stops with a `ResourceConflict` condition.
  Setting `spec.adoptExisting: true` takes them over explicitly, which is
  the intended way to resurrect a deployment after a `deletionPolicy:
  Orphan` delete.

## Getting started

### Prerequisites
- go v1.25+
- docker
- kubectl and access to a Kubernetes cluster (kind works)
- a Cloudflare API token with Workers Scripts, KV, D1, and R2 edit permissions

### Deploy

Via Helm (chart in `dist/chart`, CRD included and kept on uninstall):

```sh
make docker-build docker-push IMG=<registry>/openinspect-operator:tag
helm install openinspect-operator dist/chart -n openinspect-operator-system --create-namespace \
  --set controllerManager.container.image.repository=<registry>/openinspect-operator \
  --set controllerManager.container.image.tag=tag
kubectl create secret generic cloudflare-credentials --from-literal=apiToken=...
kubectl apply -f config/samples/openinspect_v1alpha1_openinspect.yaml
```

Or via kustomize:

```sh
make install                                        # CRDs
make docker-build docker-push IMG=<registry>/openinspect-operator:tag
make deploy IMG=<registry>/openinspect-operator:tag
```

### Tests

```sh
make test        # unit + envtest; the reconciler runs against an in-process mock Cloudflare API
make test-e2e    # kind e2e: deploys the operator + a mock Cloudflare API pod,
                 # applies a full CR, verifies provisioning and cleanup

# guarded live provider conformance (real accounts, cost-limited, opt-in):
OI_LIVE_DAYTONA=1 DAYTONA_API_KEY=... go test -tags=live ./test/live/ -run Daytona -v -timeout 45m
OI_LIVE_MODAL=1 MODAL_TOKEN_ID=... MODAL_TOKEN_SECRET=... MODAL_WORKSPACE=...   go test -tags=live ./test/live/ -run Modal -v
```

The mock Cloudflare API (`internal/cfmock`, image built from
`Dockerfile.cfmock`) implements exactly the endpoints the operator uses and
backs D1 with real SQLite, so migration semantics are genuinely exercised.
Point `spec.cloudflare.apiBaseURL` at it for hermetic testing.

### Uninstall

```sh
kubectl delete -k config/samples/   # CRs first, so finalizers clean up Cloudflare
make undeploy uninstall
```

### Provider development

See [Adding a Sandbox Provider](docs/ADDING_SANDBOX_PROVIDER.md) for the full
SDK/client, CRD, ownership, finalizer, testing, and release checklist. Modal
and Daytona are currently the only fully supported providers.

## License

Apache-2.0
