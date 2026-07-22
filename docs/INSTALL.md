# Installing OpenInspect on any Kubernetes cluster

This guide takes you from an empty cluster to a running
[Open-Inspect](https://github.com/ColeMurray/background-agents) installation.
The operator is an installer for one pinned upstream revision: applying one
`OpenInspect` custom resource provisions all Cloudflare resources (D1 with
schema migrations, KV, R2, the workers with **embedded release bundles**),
seeds the platform's global secrets store, builds the provider base
artifact, deploys the upstream Modal app where applicable, and runs the web
UI in-cluster.

You never run Terraform, `modal deploy`, `wrangler`, or snapshot builds by
hand, and you never create worker/migration ConfigMaps.

## 1. Prerequisites

| What | Why | Where |
|---|---|---|
| Kubernetes cluster ≥ 1.29 + `kubectl` + `helm` ≥ 3.8 | runs the operator, the build/deploy Jobs, and the web UI | anywhere (kind works) |
| Cloudflare account + API token | D1, KV, R2, Workers | [dash.cloudflare.com](https://dash.cloudflare.com/profile/api-tokens) — create a token with **Account** permissions: `Workers Scripts:Edit`, `Workers KV Storage:Edit`, `D1:Edit`, `Workers R2 Storage:Edit` |
| GitHub App (+ OAuth credentials) | repo access for sandboxes, user login | [github.com/settings/apps](https://github.com/settings/apps) — see [1a](#1a-github-app) |
| Anthropic API key | the coding agent inside sandboxes | [console.anthropic.com](https://console.anthropic.com/) |
| **Either** Modal account + API token | Modal sandbox provider | [modal.com](https://modal.com/settings/tokens) |
| **or** Daytona account + API key | Daytona sandbox provider | [app.daytona.io](https://app.daytona.io/) — key needs **Sandboxes: R/W/D** and **Snapshots: R/W/D** |

The only things the operator cannot automate are account signups and OAuth
application registrations — everything else is the CR.

### 1a. GitHub App

Create a GitHub App (upstream's runtime uses it to clone/push and to
authenticate users):

1. **GitHub → Settings → Developer settings → GitHub Apps → New GitHub App.**
2. Repository permissions: `Contents: Read and write`,
   `Pull requests: Read and write`, `Metadata: Read-only`.
3. Enable **OAuth** ("Request user authorization during installation" /
   callback URL = your web UI URL + `/api/auth/callback/github`).
4. Generate a **private key** (PEM) and a **client secret**.
5. **Install the App** on the org/repos you want sandboxes to work on and
   note the **installation id** (the number in the installation URL).

You now have: *App ID*, *private key (PEM)*, *installation id*, *client id*,
*client secret*.

## 2. Install the operator (Helm)

Releases publish the chart, the operator image, the sandbox runtime image,
and the modal-deploy image together; the chart's defaults reference its own
release, so no image overrides are needed:

```sh
helm install openinspect-operator \
  oci://ghcr.io/dirien/charts/openinspect-operator \
  --version <X.Y.Z> \
  -n openinspect-operator-system --create-namespace
```

The CRD is installed by the chart and kept on uninstall. Verify:

```sh
kubectl -n openinspect-operator-system get deploy openinspect-operator-controller-manager
kubectl get crd openinspects.openinspect.dirien.dev
```

> Air-gapped / mirrored registries: override
> `controllerManager.container.image.repository` (+`.tag`),
> `controllerManager.container.runtimeBuilderImage`, and
> `controllerManager.container.modalDeployImage`.

## 3. Create the namespace and secrets

Each installation lives in its own namespace with its own credentials — the
operator has no cluster-level tokens.

```sh
kubectl create namespace open-inspect
```

### 3a. Cloudflare credentials (all providers)

```sh
kubectl -n open-inspect create secret generic cloudflare-credentials \
  --from-literal=apiToken='<CLOUDFLARE_API_TOKEN>'
```

### 3b. Platform secrets — common part (all providers)

Generate the internal random secrets first:

```sh
TOKEN_ENCRYPTION_KEY=$(openssl rand -hex 32)
REPO_SECRETS_ENCRYPTION_KEY=$(openssl rand -hex 32)
INTERNAL_CALLBACK_SECRET=$(openssl rand -hex 32)
NEXTAUTH_SECRET=$(openssl rand -hex 32)
```

| Key | Value |
|---|---|
| `githubClientSecret` | GitHub App OAuth client secret |
| `githubAppId` | GitHub App ID (number) |
| `githubAppPrivateKey` | GitHub App private key (full PEM) |
| `githubAppInstallationId` | GitHub App installation id (number) |
| `tokenEncryptionKey` | random (above) — encrypts stored user tokens |
| `repoSecretsEncryptionKey` | random (above) — encrypts repo-scoped secrets |
| `internalCallbackSecret` | random (above) — authenticates service-to-service calls |
| `nextauthSecret` | random (above) — web UI session signing |
| `anthropicApiKey` | your Anthropic API key |

### 3c. Provider part

**Modal** — from [modal.com/settings/tokens](https://modal.com/settings/tokens)
plus one more random secret (`modalApiSecret` authenticates the control
plane against the deployed Modal app; you invent it):

```sh
MODAL_API_SECRET=$(openssl rand -hex 32)

kubectl -n open-inspect create secret generic platform-secrets \
  --from-literal=githubClientSecret='<GH_APP_CLIENT_SECRET>' \
  --from-literal=githubAppId='<GH_APP_ID>' \
  --from-literal=githubAppInstallationId='<GH_APP_INSTALLATION_ID>' \
  --from-file=githubAppPrivateKey=<path/to/github-app-key.pem> \
  --from-literal=tokenEncryptionKey="$TOKEN_ENCRYPTION_KEY" \
  --from-literal=repoSecretsEncryptionKey="$REPO_SECRETS_ENCRYPTION_KEY" \
  --from-literal=internalCallbackSecret="$INTERNAL_CALLBACK_SECRET" \
  --from-literal=nextauthSecret="$NEXTAUTH_SECRET" \
  --from-literal=anthropicApiKey='<ANTHROPIC_API_KEY>' \
  --from-literal=modalTokenId='<MODAL_TOKEN_ID>' \
  --from-literal=modalTokenSecret='<MODAL_TOKEN_SECRET>' \
  --from-literal=modalApiSecret="$MODAL_API_SECRET"
```

**Daytona** — replace the three `modal*` lines with:

```sh
  --from-literal=daytonaApiKey='<DAYTONA_API_KEY>'
```

(Secret and key names are free-form — the CR references them explicitly;
the names above match the samples.)

## 4. Apply the custom resource

Start from the samples and adjust `deploymentName` (unique per Cloudflare
account, immutable), `cloudflare.accountId`, and the web section:

- Modal: [`config/samples/openinspect_v1alpha1_openinspect_modal.yaml`](../config/samples/openinspect_v1alpha1_openinspect_modal.yaml)
- Daytona: [`config/samples/openinspect_v1alpha1_openinspect_daytona.yaml`](../config/samples/openinspect_v1alpha1_openinspect_daytona.yaml)

What you do **not** configure: worker script sources and D1 migrations
(embedded in the release), the Daytona base snapshot (built and managed by
the operator; `daytona.baseSnapshot` is only for adopting an existing
external snapshot), the Modal app deployment or its endpoints (the operator
deploys the pinned upstream app with an owned Job and discovers the
endpoints into `status.modalApp`).

Provider-specific spec requirements:

- **Modal** (`spec.sandbox.provider: modal`): the operator feeds the
  upstream app's three named Modal secrets from your CR, so the spec MUST
  contain an `appSecrets` entry with key `ANTHROPIC_API_KEY` and the
  control-plane worker MUST bind `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`,
  `GITHUB_APP_INSTALLATION_ID`, and `INTERNAL_CALLBACK_SECRET` (the samples
  do). Use one Modal *environment* per installation
  (`spec.sandbox.modal.environment`); a second CR targeting the same
  workspace+environment is refused.
- **Daytona** (`spec.sandbox.provider: daytona`): `daytona.apiUrl` (usually
  `https://app.daytona.io/api`) and the API key ref. Nothing else.

```sh
kubectl apply -f my-openinspect.yaml
kubectl -n open-inspect get oi -w
```

## 5. What happens next (and how to watch it)

```sh
kubectl -n open-inspect describe oi <name>       # conditions + events
kubectl -n open-inspect get jobs                 # runtime-build / modal-deploy Jobs
kubectl -n open-inspect logs job/<name>-rtb-...  # live provider build logs
```

| Condition | Meaning |
|---|---|
| `D1Ready` | database created, embedded migrations applied |
| `StorageReady` | KV namespaces + R2 buckets |
| `WorkersReady` | embedded worker bundles deployed |
| `RuntimeReady` | provider base artifact exists and matches the release digest (Daytona: built by the runtime-build Job, typically 3–5 min; artifact in `status.sandboxRuntime`) |
| `ProviderAppReady` | Modal: upstream app deployed by the modal-deploy Job, endpoints discovered and healthy (`status.modalApp`); Daytona: trivially true |
| `AppSecretsReady` | `spec.appSecrets` seeded into the platform's global secrets store |
| `WebReady` | web Deployment/Service applied |
| `Ready` | everything above |

A fresh Daytona installation reaches `Ready=True` in a few minutes; Modal
adds the app deploy (a one-time Job run per release/secret change). Reach
the web UI with `kubectl -n open-inspect port-forward svc/<name>-web 3000:80`
or your own Ingress.

## 6. Web UI image

Nothing to build: with `spec.web.image` empty, the operator deploys the
release's web image for your provider
(`ghcr.io/dirien/openinspect-web:<vX.Y.Z>-modal|daytona`, built from the
pinned unmodified upstream source) and wires the deployment-specific URLs
into it at container start (`OI_WS_URL`, `CONTROL_PLANE_URL`). Renaming or
re-pointing a deployment is a rollout, not a rebuild.

You still provide the app-level runtime env shown in the samples
(`NEXTAUTH_URL`, `GITHUB_CLIENT_ID`, `ALLOWED_USERS`) and `secretEnv`
(`NEXTAUTH_SECRET`, `GITHUB_CLIENT_SECRET`, `INTERNAL_CALLBACK_SECRET`,
`ANTHROPIC_API_KEY`); anything you set explicitly wins over injected
values.

Set `spec.web.image` only to bring your own build (e.g. custom branding
baked via `NEXT_PUBLIC_APP_NAME`); the operator then applies stricter pod
hardening (read-only root filesystem, forced non-root uid) since your image
does not need the startup URL rewrite.

## 7. Updates, teardown, and safety rails

- **Operator upgrade**: `helm upgrade` to a new chart version. If the new
  release changed the runtime bundle, the operator builds the new
  digest-named artifact, flips the reference only when it is ready, and
  garbage-collects the old one after a 24 h rollback grace period
  (`spec.sandbox.runtime.updatePolicy: Never` holds the current artifact).
- **Teardown**: `kubectl delete oi <name>` removes everything —
  Cloudflare resources, owned provider sandboxes (D1 session-index
  evidence), recorded prebuild images, the managed base artifact, and the
  deployed Modal app. `spec.deletionPolicy: Orphan` skips all external
  cleanup; `spec.sandbox.runtime.deletionPolicy: Orphan` retains only the
  managed artifacts (references are recorded in events).
- **Safety**: existing remote resources under this CR's names are never
  captured silently (`ResourceConflict` unless `spec.adoptExisting: true`);
  duplicate `deploymentName`s on one account are refused; unknown provider
  state never counts as successful deletion — the finalizer stays until
  cleanup is confirmed.

## 8. Troubleshooting

| Symptom | Likely cause |
|---|---|
| `Ready=False (InvalidSpec)` | cross-field spec error — the message names it (e.g. Modal without the `ANTHROPIC_API_KEY` appSecret or GitHub App bindings) |
| `RuntimeReady=False (RuntimeBuildFailed)` | provider build failed — `kubectl logs job/<name>-rtb-...`; the Job is recreated after a 10 min cool-down |
| `ProviderAppReady=False (ModalDeployFailed)` | `kubectl logs job/<name>-mdp-...` (Modal CLI output) |
| `ProviderAppReady=False (ModalEnvironmentConflict)` | another CR already manages that Modal workspace+environment — use a separate environment |
| `Ready=False (ResourceConflict)` | remote resource with this name already exists — set `spec.adoptExisting: true` to take it over deliberately |
| `CredentialsUnavailable` on delete | referenced Secret is gone — restore it, or set `spec.deletionPolicy: Orphan` |
