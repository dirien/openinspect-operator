#!/bin/sh
# Runtime wiring for the release web image.
#
# Next.js inlines NEXT_PUBLIC_* into the compiled bundles at build time, so
# a generic image cannot carry the deployment's control-plane WebSocket URL.
# The image is built with a unique placeholder instead; this entrypoint
# substitutes it with $OI_WS_URL across the compiled output (client chunks
# AND the SSR chunks, which carry the same inlined value) and then starts
# the server. The upstream source itself is never modified.
set -eu

PLACEHOLDER="wss://oi-runtime-ws-url-placeholder.invalid"

if [ -z "${OI_WS_URL:-}" ]; then
  echo "web-entrypoint: OI_WS_URL is not set; the browser cannot reach the control plane without it" >&2
  echo "web-entrypoint: set OI_WS_URL to the control-plane WebSocket URL (wss://<worker-host>)" >&2
  exit 1
fi

case "$OI_WS_URL" in
  *"|"*)
    # The substitution uses | as the sed delimiter; a | in a URL is invalid
    # anyway (RFC 3986), so refuse instead of corrupting the bundle.
    echo "web-entrypoint: OI_WS_URL must not contain '|'" >&2
    exit 1
    ;;
esac

# Escape sed replacement metacharacters (& and \).
replacement=$(printf '%s' "$OI_WS_URL" | sed -e 's/[&\\]/\\&/g')

substituted=0
for f in $(grep -rl "$PLACEHOLDER" packages/web/.next 2>/dev/null); do
  sed -i "s|$PLACEHOLDER|$replacement|g" "$f"
  substituted=$((substituted + 1))
done
echo "web-entrypoint: wired OI_WS_URL into $substituted bundle file(s)"

exec node packages/web/server.js
