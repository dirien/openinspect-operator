<!-- FOR AI AGENTS - Human readability is a side effect, not a goal -->
<!-- Managed by agent: keep sections and order; edit content, not structure -->
<!-- Last updated: 2026-07-17 -->

# AGENTS.md

**Precedence:** the **closest `AGENTS.md`** to the files you're changing wins. Root holds global defaults only.

## What This Operator Does

One CR `OpenInspect` (`openinspect.dirien.dev/v1alpha1`, shortname `oi`) provisions a full
[ColeMurray/background-agents](https://github.com/ColeMurray/background-agents) ("Open-Inspect")
installation: Cloudflare D1 (+embedded SQL migrations), KV, R2, Workers (embedded release bundles),
the web UI as an in-cluster Deployment+Service, sandbox-provider wiring, the platform's global
secrets store, the managed Daytona base snapshot (runtime-build Job), and the upstream Modal app
(modal-deploy Job + named Modal secrets + endpoint discovery). The operator is an installer for one
pinned upstream revision (`third_party/background-agents/UPSTREAM_COMMIT`); upstream is never
patched (EPIC1 D1–D8), and upstream's control plane drives all session sandboxes at runtime.
One CR = one isolated installation; `spec.deploymentName` suffixes every Cloudflare resource name.
CRD conditions: `Ready`, `D1Ready`, `StorageReady`, `RuntimeReady`, `ProviderAppReady`,
`WorkersReady`, `WebReady`, `AppSecretsReady`.

## Commands

| Task | Command |
|------|---------|
| Test (envtest suite) | `make test` (~3.5min) |
| Lint / auto-fix | `make lint` / `make lint-fix` |
| Regenerate CRDs/RBAC after marker edits | `make manifests` |
| Regenerate DeepCopy after type edits | `make generate` |
| Build image | `make docker-build IMG=<img>` |
| Deploy to cluster | `make deploy IMG=<img>` |
| E2E (dedicated kind cluster only) | `make test-e2e` |
| Helm chart resync after API/RBAC changes | `make helm-sync` (CI fails on stale copies) |
| Re-sync pinned upstream tree | `hack/sync-upstream.sh <commit>` (`--verify` in CI) |
| Rebuild embedded worker bundles | `hack/build-bundles.sh` (`--verify` in CI; needed when upstream worker sources change) |
| Regenerate Dockerfile.runtime | `make generate-runtime` (part of check-generated) |
| Runtime digest golden after toolchain/pin changes | `go test ./internal/runtime -run TestDigestGolden -update` |
| Live provider conformance (guarded, real accounts) | `OI_LIVE_DAYTONA=1 ... go test -tags=live ./test/live/` |

## Deployment Gotchas

| Gotcha | Rule |
|--------|------|
| Worker bundles | Embedded release bundles deploy by default (no ConfigMap). For the dev override: store gzipped in ConfigMap `binaryData` with a `.gz` key (operator gunzips); create with `kubectl create`, NOT client-side `kubectl apply` (256KB annotation limit) |
| Provider CLIs | Never — Go SDK / typed HTTP only. The single scoped exception (EPIC1 D5) is the modal-deploy Job image (pinned Modal CLI + pinned upstream source, `Dockerfile.modal-deploy`) |
| Job images | runtime-build Jobs run `RELATED_IMAGE_RUNTIME_BUILDER` (operator image, ships `/runtime-builder`); modal-deploy Jobs run `RELATED_IMAGE_MODAL_DEPLOY`. Keep both in lockstep with the deployed release |
| Image deploys to kind | Always use a UNIQUE image tag and verify the binary inside the image (`docker cp` + `grep -a` for a new symbol) before debugging "unchanged" behavior |
| `spec.appSecrets` keys | Must be UPPERCASE (CRD-enforced) — the upstream store uppercases all keys on write; reserved keys (`PATH`, `HOME`, `SANDBOX_ID`, …) are rejected in `validateSpec`; provider modal additionally REQUIRES an `ANTHROPIC_API_KEY` entry plus `GITHUB_APP_*`/`INTERNAL_CALLBACK_SECRET` worker bindings (they feed the upstream app's named Modal secrets) |
| Web UI images | `NEXT_PUBLIC_WS_URL` is baked into the Next.js bundle at build time — each deployment needs its own image build; runtime env cannot change it |
| Secrets seeding | `hack/create-secrets.sh` seeds namespaces from Pulumi ESC; `config/samples/*_modal.yaml` / `*_daytona.yaml` are complete appliable examples |

## Kubebuilder Rules

- **Never edit generated files**: `config/crd/bases/*`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT`, `dist/chart` CRD/RBAC copies.
- **Never remove `// +kubebuilder:scaffold:*` markers**; scaffold with the CLI (`kubebuilder create api|webhook`), keep the standard layout.
- After editing `*_types.go` or RBAC markers: `make manifests generate` (and `make helm-sync`), commit regenerated output.
- Before claiming done: run `make test` and show the output — never claim green without evidence.

## Index of scoped AGENTS.md

| Scope | Contents |
|-------|----------|
| [internal/](./internal/AGENTS.md) | Reconciler pipeline, key files, code-level gotchas, test hooks (`AppSecretsURL` etc.), cfmock |

## References

- Kubebuilder book (layout, markers, webhooks, plugins): https://book.kubebuilder.io
- controller-runtime FAQ: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md
- Upstream platform (worker/API contracts, sandbox runtime): https://github.com/ColeMurray/background-agents
