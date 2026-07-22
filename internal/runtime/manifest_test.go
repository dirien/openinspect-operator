package runtime

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func load(t *testing.T) *Manifest {
	t.Helper()
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func TestLoadEmbedsRuntimeTree(t *testing.T) {
	m := load(t)
	paths := map[string]bool{}
	for _, f := range m.RuntimeFiles {
		paths[f.Path] = true
	}
	// Spot checks across the tree: supervisor, bridge, an underscore-prefixed
	// file (plain go:embed would drop it), a tool, a plugin, and a skill.
	for _, want := range []string{
		"entrypoint.py",
		"bridge.py",
		"__init__.py",
		"tools/_bridge-client.js",
		"plugins/inspect-plugin.js",
		"skills/agent-browser/SKILL.md",
		"bin/oi-git-sign",
		"credentials/git_credential_helper.py",
	} {
		if !paths[want] {
			t.Errorf("runtime file %q missing from embed", want)
		}
	}
	if m.UpstreamCommit == "" || len(m.UpstreamCommit) != 40 {
		t.Errorf("upstream commit %q is not a full hash", m.UpstreamCommit)
	}
}

func TestMaterializeRestoresExecutableBit(t *testing.T) {
	m := load(t)
	dir := t.TempDir()
	if err := m.Materialize(dir); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "bin", "oi-git-sign"))
	if err != nil {
		t.Fatalf("stat oi-git-sign: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("bin/oi-git-sign mode = %v, want 0755", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Join(dir, "entrypoint.py"))
	if err != nil {
		t.Fatalf("stat entrypoint.py: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("entrypoint.py mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestDockerfileRendersPinnedToolchain(t *testing.T) {
	m := load(t)
	df, err := m.Dockerfile()
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	version, err := m.SandboxVersion()
	if err != nil {
		t.Fatalf("SandboxVersion: %v", err)
	}
	for _, want := range []string{
		"FROM " + m.BaseImage,
		"opencode-ai@" + OpenCodeVersion,
		"code-server_" + CodeServerVersion + "_amd64.deb",
		"agent-browser@" + AgentBrowserVersion,
		"ttyd/releases/download/" + TtydVersion,
		TtydSHA256,
		"SANDBOX_VERSION=" + `"` + version + `"`,
		"COPY third_party/background-agents/packages/sandbox-runtime/src/sandbox_runtime/ /app/sandbox_runtime/",
		"dev.dirien.openinspect.upstream-commit=\"" + m.UpstreamCommit + "\"",
		`ENTRYPOINT ["python", "-m", "sandbox_runtime.entrypoint"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}
