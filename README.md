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
| Sandbox providers | `spec.sandbox` selects **modal / daytona / vercel / opencomputer**; the operator injects that provider's vars and secret bindings into the control-plane worker (explicit worker values win) |
| Deletion | finalizer removes all Cloudflare resources; `deletionPolicy: Orphan` keeps them |

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

Out of scope (not automatable via APIs): GitHub/Slack/Linear/Google app
registrations, and Modal/Daytona/OpenComputer sandbox image builds (CLI- and
script-driven upstream).

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
  d1:
    migrationsConfigMapRef: d1-migrations    # keys: 0001_xxx.sql, 0002_yyy.sql, ...
  kvNamespaces: [session-index]
  r2Buckets: [media]
  workers:
    - name: control-plane
      script: {configMapRef: {name: worker-bundles, key: control-plane.js}}  # or url:
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
  web:
    image: ghcr.io/your-org/open-inspect-web:latest
```

Cloudflare resource names expand to `<namePrefix>-<logical>-<deploymentName>`
(prefix defaults to `open-inspect`), matching the upstream Terraform naming.

`kubectl get oi` shows Ready state, reason, and applied migration count.

## Getting Started

### Prerequisites
- go v1.25+
- docker
- kubectl and access to a Kubernetes cluster (kind works)
- a Cloudflare API token with Workers Scripts, KV, D1, and R2 edit permissions

### Deploy

```sh
make install                                        # CRDs
make docker-build docker-push IMG=<registry>/openinspect-operator:tag
make deploy IMG=<registry>/openinspect-operator:tag
kubectl create secret generic cloudflare-credentials --from-literal=apiToken=...
kubectl apply -f config/samples/openinspect_v1alpha1_openinspect.yaml
```

### Tests

```sh
make test        # unit + envtest; the reconciler runs against an in-process mock Cloudflare API
make test-e2e    # kind e2e: deploys the operator + a mock Cloudflare API pod,
                 # applies a full CR, verifies provisioning and cleanup
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

## License

Apache-2.0
