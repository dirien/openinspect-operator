#!/usr/bin/env bash
# Build the platform worker bundles from the pinned upstream tree and embed
# them under internal/platformbundle (EPIC1 section 7). Users never create
# worker-bundles ConfigMaps; releases embed exactly these bytes.
#
# The output is deterministic for a given pinned tree: esbuild is pinned by
# upstream's package-lock.json and gzip runs with -n (no name/timestamp), so
# CI can rebuild and byte-compare.
#
# Usage: hack/build-bundles.sh [--verify]
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
UPSTREAM="$REPO_ROOT/third_party/background-agents"
OUT="$REPO_ROOT/internal/platformbundle"
WORKERS=(control-plane github-bot slack-bot linear-bot)

MODE=build
[ "${1:-}" = "--verify" ] && MODE=verify

command -v node >/dev/null || { echo "node >= 22 is required" >&2; exit 1; }

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

# Build in a COPY of the pinned tree: node_modules must never land inside
# the repository (dependencies ship stray .go files that would break Go
# tooling walking ./...).
SRC="$STAGE/src"
mkdir -p "$SRC"
rsync -a --exclude node_modules --exclude 'packages/*/dist' "$UPSTREAM/" "$SRC/"
cd "$SRC"

# --ignore-scripts: no lifecycle scripts run from upstream dependencies at
# install time (esbuild ships prebuilt binaries via optionalDependencies).
CI=1 npm ci --ignore-scripts --no-audit --no-fund >&2

# Workers resolve @open-inspect/shared through its dist/ output.
npm run build -w @open-inspect/shared >&2

for worker in "${WORKERS[@]}"; do
  npm run build -w "@open-inspect/$worker" >&2
  gzip -n -9 -c "packages/$worker/dist/index.js" > "$STAGE/$worker.js.gz"
done

ESBUILD_VERSION=$(node -p "require('esbuild/package.json').version")
UPSTREAM_COMMIT=$(cat UPSTREAM_COMMIT)

# metadata.json: content fingerprints of the UNCOMPRESSED bundles plus the
# toolchain that produced them. No timestamps — the file must be stable.
node - "$STAGE" "$UPSTREAM_COMMIT" "$ESBUILD_VERSION" "${WORKERS[@]}" >"$STAGE/metadata.json" <<'EOF'
const [stage, commit, esbuild, ...workers] = process.argv.slice(2);
const { createHash } = require("node:crypto");
const { readFileSync } = require("node:fs");
const { gunzipSync } = require("node:zlib");
const bundles = {};
for (const w of workers.sort()) {
  const raw = gunzipSync(readFileSync(`${stage}/${w}.js.gz`));
  bundles[w] = { sha256: createHash("sha256").update(raw).digest("hex"), bytes: raw.length };
}
process.stdout.write(JSON.stringify({ upstreamCommit: commit, esbuild, bundles }, null, 2) + "\n");
EOF

if [ "$MODE" = verify ]; then
  status=0
  for f in "${WORKERS[@]/%/.js.gz}" metadata.json; do
    cmp -s "$STAGE/$f" "$OUT/$f" || { echo "stale: internal/platformbundle/$f" >&2; status=1; }
  done
  [ "$status" -eq 0 ] || { echo "run: hack/build-bundles.sh" >&2; exit 1; }
  echo "internal/platformbundle matches a fresh build of $UPSTREAM_COMMIT"
  exit 0
fi

mkdir -p "$OUT"
for f in "${WORKERS[@]/%/.js.gz}" metadata.json; do
  cp "$STAGE/$f" "$OUT/$f"
done
echo "built ${#WORKERS[@]} bundles from $UPSTREAM_COMMIT into internal/platformbundle"
