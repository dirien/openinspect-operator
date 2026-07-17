#!/usr/bin/env bash
# Seed global secrets into OpenInspect deployments via the control-plane
# service API (PUT /secrets, HMAC auth with INTERNAL_CALLBACK_SECRET).
# No web-UI clickops needed; values are encrypted at rest by the app.
#
# Usage: hack/seed-global-secrets.sh [deployment ...]
#   default deployments: daytona modal
#   Worker URL pattern: https://open-inspect-control-plane-<name>.engin-diri.workers.dev
set -euo pipefail

DEPLOYMENTS=("$@")
[ $# -eq 0 ] && DEPLOYMENTS=(daytona modal)

OI=$(pulumi env open dirien/open-inspect-prod)
ICS=$(jq -r '.pulumiConfig["open-inspect:internalCallbackSecret"]' <<<"$OI")
ANTHROPIC=$(pulumi env open pulumi-idp/auth | jq -r '.environmentVariables._ANTHROPIC_API_KEY_')

for D in "${DEPLOYMENTS[@]}"; do
  TS=$(python3 -c 'import time; print(int(time.time()*1000))')
  SIG=$(printf '%s' "$TS" | openssl dgst -sha256 -hmac "$ICS" -hex | awk '{print $NF}')
  jq -n --arg k "$ANTHROPIC" '{secrets: {ANTHROPIC_API_KEY: $k}}' |
    curl -sf -X PUT \
      -H "Authorization: Bearer $TS.$SIG" \
      -H "Content-Type: application/json" \
      "https://open-inspect-control-plane-$D.engin-diri.workers.dev/secrets" \
      -d @- | jq -c "{deployment: \"$D\", result: .}"
done
