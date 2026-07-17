<!-- FOR AI AGENTS - Human readability is a side effect, not a goal -->
<!-- Managed by agent: keep sections and order; edit content, not structure -->
<!-- Last updated: 2026-07-17 -->

# internal/ — reconciler, Cloudflare client, CF mock

## Overview

Go packages implementing the operator: `controller/` (reconciliation), `cloudflare/`
(idempotent cloudflare-go v7 wrapper), `cfmock/` (mock Cloudflare API for tests).

## Key Files

| File | Purpose |
|------|---------|
| `controller/openinspect_controller.go` | Reconcile pipeline: validate → claim-conflict → D1 → storage → workers → web → appSecrets; failures via `r.failed()` |
| `controller/sandbox.go` | Injects provider vars/secrets (`modal`\|`daytona`\|`vercel`\|`opencomputer`) into the control-plane worker; explicit worker values win |
| `controller/appsecrets.go` | Converges `spec.appSecrets` into the platform's global secrets store via the control-plane worker's `/secrets` API (HMAC `timestamp.hex` bearer signed with `INTERNAL_CALLBACK_SECRET`); GET keys → PUT on change/missing → DELETE removed; never touches keys added out of band |
| `controller/validate.go` | Terminal `InvalidSpec` checks, incl. appSecrets reserved keys and required ICS binding |
| `controller/modalsweep.go` | CR deletion uses Modal's Go SDK/API to sweep sandboxes and stop the env's app; no upstream web endpoints |
| `controller/daytonasweep.go` | Daytona SDK cleanup, scoped by CR-owned D1 session IDs plus upstream sandbox labels |
| `cloudflare/cf.go` | Idempotent CF SDK wrapper (`maxListItems` pager guard) |
| `cfmock/server.go` | Mock Cloudflare API (real SQLite for D1); used by envtest and kind e2e (`Dockerfile.cfmock`, `spec.cloudflare.apiBaseURL` override) |

## Setup & environment

`make setup-envtest` fetches the envtest binaries into `bin/k8s/` (needed once; `make test` does it).

## Build & tests

- Full suite: `make test` (~3.5min, envtest + Ginkgo/Gomega, see `controller/suite_test.go`).
- Single test: `go test ./internal/controller/ -run <Name>` (needs absolute `KUBEBUILDER_ASSETS` for envtest specs — prefer `make test`).
- Reconciler test hooks: `NewCF`, `ModalCleanup`, `DaytonaCleanup`, `AppSecretsURL` — substitute provider cleanup or point HTTP paths at `httptest` servers (see `controller/appsecrets_integration_test.go`).
- E2E: `make test-e2e` against a dedicated kind cluster only.

## Code style & conventions

- Primary object and `persistStatus` MUST use live API reads (`mgr.GetAPIReader()`); a cached read can persist stale status and wipe recorded resource IDs.
- Status uses `metav1.Condition`; condition reasons are machine API — renaming is a breaking change. Remove a feature's condition when its spec section is removed (see `controller/web.go`, `controller/appsecrets.go`).
- cloudflare-go v7 auto-pager stops only on an EMPTY page — list mocks must return empty for `page >= 2` or `iter.Next()` hangs; keep the `maxListItems` guard (Daytona sweep: `maxDaytonaSweepItems`).
- Deletion order is load-bearing: workers first (stops new sessions), then provider sweep (reads D1 session evidence), KV, R2, D1 LAST. `D1SessionIDs` returns `found=false` for a missing database/table so deletion degrades to a skipped sweep (`DaytonaSweepSkipped`) instead of wedging the finalizer.
- `cleanupModalApp` resolves the app by environment NAME; a web suffix on the defaulted `main` environment is rejected in `validateSandbox`, and app-not-found cleanup emits `ModalAppNotFound` — never a fabricated `ModalAppStopped`.

## Security & safety

- Never embed secret values in errors, events, or logs.
- `status.appSecrets.appliedHash` is HMAC-keyed so low-entropy values cannot be dictionary-attacked from CR read access — keep it keyed.
- External-store convergence must only delete keys this CR seeded (tracked in `status.appSecrets.keys`).

## Examples (golden samples)

- `controller/appsecrets.go` + `controller/appsecrets_integration_test.go` — external-store convergence with status hash-gating, condition lifecycle, and an `httptest`-backed integration spec.
- `controller/modalsweep.go` — direct provider cleanup with Modal's SDK and authenticated API.
- `controller/daytonasweep.go` — fail-closed external-resource ownership from provider labels intersected with D1 session IDs.
- `controller/web.go` — owned Deployment/Service with condition removal when the spec section disappears.

## PR/commit checklist

1. `make lint-fix`
2. `make manifests generate` after type/marker edits; commit regenerated output
3. `make test` — paste output as evidence

## When stuck

Read the upstream contracts in [ColeMurray/background-agents](https://github.com/ColeMurray/background-agents)
(`packages/control-plane/src/routes/secrets.ts`, `packages/modal-infra/src/app.py`) — the operator
mirrors their auth schemes and store semantics. Root [`AGENTS.md`](../AGENTS.md) has deployment-level gotchas.
