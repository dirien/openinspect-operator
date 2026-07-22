#!/usr/bin/env bash
# Synchronize the curated upstream source subset into third_party/background-agents.
#
# The operator is an installer for one exact upstream revision: every release
# pins the commit recorded in third_party/background-agents/UPSTREAM_COMMIT
# and never fetches upstream main at runtime. This script is the only way the
# pinned tree changes.
#
# Usage:
#   hack/sync-upstream.sh <commit>     sync third_party to <commit>
#   hack/sync-upstream.sh              re-sync the recorded commit (repair)
#   hack/sync-upstream.sh --verify     fail if third_party does not byte-match
#                                      a fresh sync of the recorded commit (CI)
#
# UPSTREAM_REPO overrides the source repository (e.g. a local mirror).
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEST="$REPO_ROOT/third_party/background-agents"
UPSTREAM_REPO=${UPSTREAM_REPO:-https://github.com/ColeMurray/background-agents.git}

# Files under DEST owned by this repository, never touched by a sync.
OWNED_FILES=(embed.go README.md)

# Paths synced from upstream. Audited against what each supported provider's
# installation consumes:
#   - worker bundles:    control-plane, github-bot, slack-bot, linear-bot,
#                        shared (file: dependency), root npm manifests
#   - web image builds:  packages/web (built per deployment; NEXT_PUBLIC_*
#                        values are baked in at build time)
#   - modal-deploy Job:  packages/modal-infra (+ sandbox-runtime, a uv path
#                        dependency) driven by the pinned Modal CLI
#   - runtime manifest:  packages/sandbox-runtime (embedded into provider
#                        base artifacts), packages/daytona-infra and
#                        packages/modal-infra/src/images (version-pin parity)
#   - D1 schema:         terraform/d1/migrations
# packages/opencomputer-infra contributes only its package.json: it is an npm
# workspace member the lockfile refuses to install without, but its provider
# is not supported at this revision (EPIC1 D3).
SYNC_PATHS=(
  LICENSE
  package.json
  package-lock.json
  packages/control-plane
  packages/shared
  packages/github-bot
  packages/slack-bot
  packages/linear-bot
  packages/web
  packages/sandbox-runtime
  packages/modal-infra
  packages/daytona-infra
  packages/opencomputer-infra/package.json
  terraform/d1/migrations
)

MODE=sync
COMMIT=""
for arg in "$@"; do
  case "$arg" in
    --verify) MODE=verify ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *) COMMIT="$arg" ;;
  esac
done

if [ -z "$COMMIT" ]; then
  if [ ! -f "$DEST/UPSTREAM_COMMIT" ]; then
    echo "no commit given and $DEST/UPSTREAM_COMMIT does not exist" >&2
    exit 2
  fi
  COMMIT=$(cat "$DEST/UPSTREAM_COMMIT")
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "fetching $UPSTREAM_REPO @ $COMMIT"
git init --quiet "$WORK/upstream"
git -C "$WORK/upstream" remote add origin "$UPSTREAM_REPO"
git -C "$WORK/upstream" fetch --quiet --depth 1 origin "$COMMIT"
FULL_COMMIT=$(git -C "$WORK/upstream" rev-parse FETCH_HEAD)

STAGE="$WORK/stage"
mkdir -p "$STAGE"
git -C "$WORK/upstream" archive "$FULL_COMMIT" -- "${SYNC_PATHS[@]}" | tar -x -C "$STAGE"

# Prune test-only content: nothing in the installation path consumes it, and
# the pinned tree should carry only what the operator release actually uses.
find "$STAGE" \( -type d \( -name test -o -name tests -o -name __tests__ \) \) -prune -exec rm -rf {} +
find "$STAGE" -type f \( \
    -name '*.test.ts' -o -name '*.test.tsx' -o -name '*.spec.ts' \
    -o -name 'vitest.config.ts' -o -name 'vitest.integration.config.ts' \
    -o -name 'tsconfig.test.json' -o -name 'Dockerfile.test' \
  \) -delete

printf '%s\n' "$FULL_COMMIT" > "$STAGE/UPSTREAM_COMMIT"

# Checksum manifest over every synced file, so any local modification of the
# pinned tree is detectable without network access:
#   (cd third_party/background-agents && sha256sum -c SYNC_MANIFEST.sha256)
(cd "$STAGE" && find . -type f ! -name SYNC_MANIFEST.sha256 -print0 \
  | sort -z | xargs -0 sha256sum > SYNC_MANIFEST.sha256)

if [ "$MODE" = verify ]; then
  status=0
  if [ ! -d "$DEST" ]; then
    echo "verify: $DEST does not exist" >&2
    exit 1
  fi
  # Compare fresh sync against the committed tree, ignoring operator-owned files.
  exclude_args=()
  for f in "${OWNED_FILES[@]}"; do exclude_args+=(-x "$f"); done
  diff -r "${exclude_args[@]}" "$STAGE" "$DEST" || status=1
  if [ "$status" -ne 0 ]; then
    echo "third_party/background-agents is out of sync with upstream $FULL_COMMIT" >&2
    echo "run: hack/sync-upstream.sh $FULL_COMMIT" >&2
    exit 1
  fi
  echo "third_party/background-agents matches upstream $FULL_COMMIT"
  exit 0
fi

mkdir -p "$DEST"
# Preserve operator-owned files across the sync.
for f in "${OWNED_FILES[@]}"; do
  if [ -e "$DEST/$f" ]; then cp -a "$DEST/$f" "$STAGE/$f"; fi
done
rsync -a --delete "$STAGE/" "$DEST/"

echo "synced third_party/background-agents to $FULL_COMMIT"
echo "next: make manifests generate test, and rebuild embedded bundles if worker sources changed"
