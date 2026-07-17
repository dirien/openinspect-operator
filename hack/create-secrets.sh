#!/usr/bin/env bash
# Seed namespaces with the secrets the OpenInspect samples reference.
#
# Sources (Pulumi ESC):
#   dirien/open-inspect-prod  -> Cloudflare API token + platform secrets
#   pulumi-idp/auth           -> Daytona API key + live Modal tokens + Anthropic key
#     (its CLOUDFLARE_API_TOKEN is invalid -- do not use it)
#
# Usage: hack/create-secrets.sh [namespace ...]
#   default namespaces: open-inspect-modal open-inspect-daytona
#   MIGRATIONS_DIR=<dir> also creates the d1-migrations ConfigMap from *.sql
#   KUBE_CONTEXT=<ctx>   kubectl context (default: kind-openinspect)
set -euo pipefail

CTX="${KUBE_CONTEXT:-kind-openinspect}"
NAMESPACES=("${@:-open-inspect-modal open-inspect-daytona}")
[ $# -eq 0 ] && NAMESPACES=(open-inspect-modal open-inspect-daytona)

OI=$(pulumi env open dirien/open-inspect-prod)
IDP=$(pulumi env open pulumi-idp/auth)

oi() { jq -r ".pulumiConfig[\"open-inspect:$1\"]" <<<"$OI"; }
CF_TOKEN=$(jq -r '.environmentVariables.CLOUDFLARE_API_TOKEN' <<<"$OI")
DAYTONA_KEY=$(jq -r '.environmentVariables.DAYTONA_API_KEY' <<<"$IDP")
ANTHROPIC_KEY=$(jq -r '.environmentVariables._ANTHROPIC_API_KEY_' <<<"$IDP")
MODAL_TOKEN_ID=$(jq -r '.pulumiConfig.modalTokenId' <<<"$IDP")
MODAL_TOKEN_SECRET=$(jq -r '.pulumiConfig.modalTokenSecret' <<<"$IDP")

INSTALL_ID=$(oi githubAppInstallationId)
if [ "$INSTALL_ID" = "108982000" ]; then
  echo "githubAppInstallationId stale in ESC -> overriding with 110197521"
  INSTALL_ID=110197521
fi

for NS in "${NAMESPACES[@]}"; do
  kubectl --context "$CTX" create namespace "$NS" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f -

  kubectl --context "$CTX" -n "$NS" create secret generic cloudflare-credentials \
    --from-literal=apiToken="$CF_TOKEN" \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -

  kubectl --context "$CTX" -n "$NS" create secret generic platform-secrets \
    --from-literal=githubClientSecret="$(oi githubClientSecret)" \
    --from-literal=githubAppId="$(oi githubAppId)" \
    --from-literal=githubAppInstallationId="$INSTALL_ID" \
    --from-literal=githubAppPrivateKey="$(oi githubAppPrivateKey)" \
    --from-literal=tokenEncryptionKey="$(oi tokenEncryptionKey)" \
    --from-literal=internalCallbackSecret="$(oi internalCallbackSecret)" \
    --from-literal=repoSecretsEncryptionKey="$(oi repoSecretsEncryptionKey)" \
    --from-literal=nextauthSecret="$(oi nextauthSecret)" \
    --from-literal=anthropicApiKey="$ANTHROPIC_KEY" \
    --from-literal=modalApiSecret="$(oi modalApiSecret)" \
    --from-literal=modalTokenId="$MODAL_TOKEN_ID" \
    --from-literal=modalTokenSecret="$MODAL_TOKEN_SECRET" \
    --from-literal=daytonaApiKey="$DAYTONA_KEY" \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -

  if [ -n "${MIGRATIONS_DIR:-}" ]; then
    kubectl --context "$CTX" -n "$NS" create configmap d1-migrations \
      --from-file="$MIGRATIONS_DIR" \
      --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  fi

  echo "seeded $NS"
done
