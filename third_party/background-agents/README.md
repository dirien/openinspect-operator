# Pinned upstream: ColeMurray/background-agents

This directory holds the curated, **pinned** subset of
[ColeMurray/background-agents](https://github.com/ColeMurray/background-agents)
(“Open-Inspect”) that the operator release consumes. Upstream is MIT licensed
(see [LICENSE](LICENSE)); all files except `embed.go` and this README are
verbatim upstream content.

- `UPSTREAM_COMMIT` — the exact commit every operator release installs.
- `SYNC_MANIFEST.sha256` — checksums of every synced file
  (`sha256sum -c SYNC_MANIFEST.sha256` detects local tampering offline).
- `embed.go` — operator-owned Go embedding of the runtime tree, migrations,
  and provider image recipes.

Rules:

- **Never edit synced files by hand.** The operator never patches upstream
  (EPIC1 D2); changes land by moving the pin.
- Update with `hack/sync-upstream.sh <commit>`; CI runs
  `hack/sync-upstream.sh --verify` to prove the tree byte-matches a fresh
  sync of the recorded commit.
- The running operator never fetches upstream `main` — this tree is the only
  upstream source a release ships.

What is synced (and why) is documented in `hack/sync-upstream.sh`.
