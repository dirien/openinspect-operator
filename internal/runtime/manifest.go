// Package runtime defines the canonical, provider-neutral sandbox runtime
// bundle: the single recipe every provider base artifact is built from.
// It mirrors the pinned upstream image recipes (Daytona toolchain.py as the
// provider-artifact baseline, plus the ttyd and OpenCode dependency staging
// steps from the Modal base image) so that one toolchain or runtime change
// produces exactly one new digest for all providers.
package runtime

import (
	"fmt"
	"strings"
)

// Step is one named group of shell commands, executed in order.
type Step struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands"`
}

// Manifest is the full description of the sandbox runtime image. It is pure
// data: providers map it onto their own build APIs (Daytona declarative
// images, docker build for the release image).
type Manifest struct {
	// BaseImage is the container base every provider artifact starts from.
	BaseImage string `json:"baseImage"`
	// AptPackages and PipPackages are kept as data (not folded into Steps)
	// so providers with typed install APIs can use them directly.
	AptPackages []string `json:"aptPackages"`
	PipPackages []string `json:"pipPackages"`
	// Steps are the ordered build commands after base-image setup.
	Steps []Step `json:"steps"`
	// Env is the static image environment. SANDBOX_VERSION is intentionally
	// absent: it is derived from the digest (see SandboxVersion) and folding
	// it in would make the digest self-referential.
	Env map[string]string `json:"env"`
	// RuntimeDir is where the sandbox_runtime tree lands in the image.
	RuntimeDir string `json:"runtimeDir"`
	// WorkDir and Entrypoint match the upstream sandbox contract.
	WorkDir    string   `json:"workDir"`
	Entrypoint []string `json:"entrypoint"`
	// RuntimeFiles is the embedded sandbox_runtime tree from the pinned
	// upstream revision, sorted by path.
	RuntimeFiles []File `json:"-"`

	// UpstreamCommit records the revision RuntimeFiles came from. Not part
	// of the digest: identical content pins identical behavior even when the
	// commit moves.
	UpstreamCommit string `json:"-"`
}

// NewManifest assembles the canonical manifest from the pinned versions and
// the embedded upstream runtime files.
func NewManifest(upstreamCommit string, files []File) *Manifest {
	pipArgs := make([]string, len(pipPackages))
	for i, p := range pipPackages {
		pipArgs[i] = "'" + p + "'"
	}
	return &Manifest{
		BaseImage:   "python:" + PythonVersion + "-slim-bookworm",
		AptPackages: append([]string(nil), aptPackages...),
		PipPackages: append([]string(nil), pipPackages...),
		Steps: []Step{
			// Mirrors packages/daytona-infra/src/toolchain.py (the canonical
			// provider-artifact recipe); the ttyd and opencode-deps steps come
			// from packages/modal-infra/src/images/base.py so all providers
			// carry the full tool set.
			{Name: "system-packages", Commands: []string{
				"apt-get update",
				"apt-get install -y " + strings.Join(aptPackages, " "),
			}},
			{Name: "github-cli", Commands: []string{
				"curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg",
				"echo 'deb [arch=amd64 signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main' > /etc/apt/sources.list.d/github-cli.list",
				"apt-get update && apt-get install -y gh && rm -rf /var/lib/apt/lists/*",
			}},
			{Name: "node", Commands: []string{
				"curl -fsSL https://deb.nodesource.com/setup_" + NodeMajor + ".x | bash -",
				"apt-get install -y nodejs",
			}},
			{Name: "js-toolchain", Commands: []string{
				"npm install -g pnpm@latest",
				"curl -fsSL https://bun.sh/install | bash",
			}},
			{Name: "pip", Commands: []string{
				"python -m pip install --upgrade pip",
				"python -m pip install " + strings.Join(pipArgs, " "),
			}},
			{Name: "opencode", Commands: []string{
				"npm install -g opencode-ai@" + OpenCodeVersion,
				"npm install -g @opencode-ai/plugin@" + OpenCodeVersion + " zod",
			}},
			// Pre-stage the OpenCode plugin dependency tree so first-prompt
			// startup skips the slow npm reify; also bake it into the global
			// config dir (see upstream base.py for the full rationale).
			{Name: "opencode-deps", Commands: []string{
				"mkdir -p /app/opencode-deps",
				`echo '{"name":"opencode-tools","type":"module","dependencies":{"@opencode-ai/plugin":"` + OpenCodeVersion + `"}}' > /app/opencode-deps/package.json`,
				"cd /app/opencode-deps && npm install --ignore-scripts --no-audit --no-fund",
				"mkdir -p /root/.config/opencode",
				"cp -a /app/opencode-deps/. /root/.config/opencode/",
			}},
			{Name: "code-server", Commands: []string{
				"curl -fsSL -o /tmp/code-server.deb https://github.com/coder/code-server/releases/download/v" + CodeServerVersion + "/code-server_" + CodeServerVersion + "_amd64.deb",
				"dpkg -i /tmp/code-server.deb",
				"rm /tmp/code-server.deb",
			}},
			{Name: "ttyd", Commands: []string{
				"curl -fsSL -o /usr/local/bin/ttyd https://github.com/tsl0922/ttyd/releases/download/" + TtydVersion + "/ttyd.x86_64",
				`echo "` + TtydSHA256 + `  /usr/local/bin/ttyd" | sha256sum -c -`,
				"chmod +x /usr/local/bin/ttyd",
			}},
			{Name: "agent-browser", Commands: []string{
				"npm install -g agent-browser@" + AgentBrowserVersion,
				"agent-browser install",
			}},
			{Name: "directories", Commands: []string{
				"mkdir -p /workspace /app /tmp/opencode",
			}},
			// Each git invocation delegates credentials to the runtime helper,
			// which mints fresh per-request tokens from the control plane.
			{Name: "git-credential-helper", Commands: []string{
				`printf '%s\n' '#!/bin/sh' 'exec python3 -m sandbox_runtime.credentials.git_credential_helper "$@"' > /usr/local/bin/oi-git-credentials`,
				"chmod 0755 /usr/local/bin/oi-git-credentials",
				"git config --system credential.helper /usr/local/bin/oi-git-credentials",
				"git config --system credential.useHttpPath true",
			}},
		},
		Env: map[string]string{
			"HOME":       "/root",
			"NODE_ENV":   "development",
			"PATH":       "/root/.bun/bin:/usr/local/bin:/usr/bin:/bin",
			"PYTHONPATH": "/app",
			"NODE_PATH":  "/usr/lib/node_modules",
		},
		RuntimeDir:     "/app/sandbox_runtime",
		WorkDir:        "/workspace",
		Entrypoint:     []string{"python", "-m", "sandbox_runtime.entrypoint"},
		RuntimeFiles:   files,
		UpstreamCommit: upstreamCommit,
	}
}

// Load returns the canonical manifest built from the embedded pinned
// upstream revision.
func Load() (*Manifest, error) {
	files, err := loadRuntimeFiles()
	if err != nil {
		return nil, fmt.Errorf("load runtime files: %w", err)
	}
	return NewManifest(upstreamCommit(), files), nil
}
