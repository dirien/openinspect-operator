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
| A web UI image for your deployment | `NEXT_PUBLIC_*` values are baked at build time by upstream's design | built from the pinned upstream `packages/web` (see [6](#6-web-ui-image)) |

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

Upstream bakes `NEXT_PUBLIC_*` values into the web bundle at build time (in
particular `NEXT_PUBLIC_WS_URL`, the control-plane WebSocket URL), so each
deployment builds its own image from the pinned upstream source
(`third_party/background-agents/packages/web`). Upstream ships no
self-hosting Dockerfile; a standard Next.js standalone build works:

```dockerfile
# Dockerfile.web — build from third_party/background-agents/
FROM node:22-slim AS build
ARG NEXT_PUBLIC_WS_URL
ARG NEXT_PUBLIC_SANDBOX_PROVIDER
WORKDIR /app
COPY package.json package-lock.json ./
COPY packages/web packages/web
COPY packages/shared packages/shared
COPY packages/*/package.json packages/
RUN CI=1 npm ci --ignore-scripts && npm run build -w @open-inspect/shared
ENV NEXT_PUBLIC_WS_URL=$NEXT_PUBLIC_WS_URL \
    NEXT_PUBLIC_SANDBOX_PROVIDER=$NEXT_PUBLIC_SANDBOX_PROVIDER
RUN npm run build -w @open-inspect/web

FROM node:22-slim
WORKDIR /app
COPY --from=build /app/packages/web/.next/standalone ./
COPY --from=build /app/packages/web/.next/static packages/web/.next/static
COPY --from=build /app/packages/web/public packages/web/public
EXPOSE 3000
CMD ["node", "packages/web/server.js"]
```

```sh
cd third_party/background-agents
docker build -f ../../Dockerfile.web \
  --build-arg NEXT_PUBLIC_WS_URL=wss://open-inspect-control-plane-<deploymentName>.<account-subdomain>.workers.dev \
  --build-arg NEXT_PUBLIC_SANDBOX_PROVIDER=<modal|daytona> \
  -t <registry>/open-inspect-web:<tag> .
docker push <registry>/open-inspect-web:<tag>
```

Reference it in `spec.web.image`, and set the runtime env
(`CONTROL_PLANE_URL`, `NEXTAUTH_URL`, `GITHUB_CLIENT_ID`, `ALLOWED_USERS`)
plus `secretEnv` (`NEXTAUTH_SECRET`, `GITHUB_CLIENT_SECRET`,
`INTERNAL_CALLBACK_SECRET`, `ANTHROPIC_API_KEY`) as shown in the samples.
(Your workers.dev account subdomain is shown in the Cloudflare dashboard
under Workers.)

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
