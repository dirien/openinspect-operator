// Package upstream embeds the pinned, curated subset of
// ColeMurray/background-agents ("Open-Inspect") that the operator release
// consumes at runtime: the provider-agnostic sandbox runtime, the D1 schema
// migrations, and the provider image recipes used for version-pin parity
// checks.
//
// Everything in this directory except embed.go and README.md is written by
// hack/sync-upstream.sh from the exact commit recorded in UPSTREAM_COMMIT and
// must never be edited by hand (EPIC1 D2: upstream is never modified).
// Upstream is MIT licensed; see LICENSE.
package upstream

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed UPSTREAM_COMMIT
var commitFile string

// The all: prefix is required: the runtime tree contains underscore-prefixed
// files (__init__.py, tools/_bridge-client.js) that plain go:embed skips.
//
//go:embed all:packages/sandbox-runtime/src/sandbox_runtime
var sandboxRuntime embed.FS

//go:embed terraform/d1/migrations/*.sql
var migrations embed.FS

// Provider image recipes, embedded so tests can prove the operator's pinned
// toolchain versions match this exact upstream revision.
//
//go:embed packages/daytona-infra/src/toolchain.py packages/modal-infra/src/images/base.py
var recipes embed.FS

// Commit returns the full upstream commit hash this tree was synced from.
func Commit() string {
	return strings.TrimSpace(commitFile)
}

// SandboxRuntimeFS returns the sandbox_runtime package tree, rooted so that
// fs paths are relative to the package directory (e.g. "entrypoint.py",
// "bin/oi-git-sign").
func SandboxRuntimeFS() (fs.FS, error) {
	return fs.Sub(sandboxRuntime, "packages/sandbox-runtime/src/sandbox_runtime")
}

// MigrationsFS returns the D1 schema migrations, rooted so that fs paths are
// the bare migration filenames (e.g. "0001_init.sql").
func MigrationsFS() (fs.FS, error) {
	return fs.Sub(migrations, "terraform/d1/migrations")
}

// RecipeFile returns one of the embedded provider image recipes by its
// repository-relative path.
func RecipeFile(path string) ([]byte, error) {
	return recipes.ReadFile(path)
}
