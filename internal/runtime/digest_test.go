package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden digest pins the exact runtime bundle a release ships. It only
// changes when the toolchain pins, recipe steps, or upstream runtime files
// change — every such change is a new provider artifact, so it must be a
// conscious commit: regenerate with
//
//	go test ./internal/runtime -run TestDigestGolden -update
const goldenDigestFile = "testdata/manifest-digest.txt"

func TestDigestGolden(t *testing.T) {
	m := load(t)
	got, err := m.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenDigestFile), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenDigestFile, []byte(got+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenDigestFile)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("runtime digest changed:\n  got  %s\n  want %s\n"+
			"If the toolchain or upstream pin changed intentionally, regenerate with -update.", got, strings.TrimSpace(string(want)))
	}
}

func TestDigestDeterministic(t *testing.T) {
	a := load(t)
	b := load(t)
	da, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("two loads produced different digests: %s vs %s", da, db)
	}
	if !strings.HasPrefix(da, "sha256:") || len(da) != len("sha256:")+64 {
		t.Errorf("digest %q is not sha256:<64 hex>", da)
	}
}

func TestDigestSensitivity(t *testing.T) {
	base := load(t)
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*Manifest){
		"file content": func(m *Manifest) { m.RuntimeFiles[0].Data = append([]byte("x"), m.RuntimeFiles[0].Data...) },
		"file mode":    func(m *Manifest) { m.RuntimeFiles[0].Mode = 0o700 },
		"step command": func(m *Manifest) { m.Steps[0].Commands[0] += " # changed" },
		"env value":    func(m *Manifest) { m.Env["NODE_ENV"] = "production" },
		"base image":   func(m *Manifest) { m.BaseImage = "python:3.13-slim-bookworm" },
		"entrypoint":   func(m *Manifest) { m.Entrypoint = []string{"python3", "-m", "sandbox_runtime.entrypoint"} },
	}
	for name, mutate := range mutations {
		m := load(t) // fresh copy: files are shared slices otherwise
		mutate(m)
		d, err := m.Digest()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d == baseDigest {
			t.Errorf("mutating %s did not change the digest", name)
		}
	}
}

func TestSandboxVersionDerivedFromDigest(t *testing.T) {
	m := load(t)
	short, err := m.ShortDigest()
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.SandboxVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != "oi-"+short {
		t.Errorf("SandboxVersion %q != oi-%s", v, short)
	}
	if _, tainted := m.Env["SANDBOX_VERSION"]; tainted {
		t.Error("SANDBOX_VERSION must not be part of the digested env (self-referential)")
	}
}
