package platformbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
)

func TestEmbeddedBundlesMatchMetadata(t *testing.T) {
	meta, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The bundles and the embedded runtime/migrations must come from the
	// same pinned revision — the platform advances atomically.
	if meta.UpstreamCommit != sandboxruntime.UpstreamCommit() {
		t.Errorf("bundle metadata pins %s but third_party pins %s — rerun hack/build-bundles.sh",
			meta.UpstreamCommit, sandboxruntime.UpstreamCommit())
	}
	names, err := Names()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"control-plane", "github-bot", "linear-bot", "slack-bot"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("bundle names = %v, want %v", names, want)
	}
	for _, name := range names {
		if !Has(name) {
			t.Errorf("Has(%q) = false", name)
		}
		body, err := Script(name)
		if err != nil {
			t.Fatalf("Script(%q): %v", name, err)
		}
		sum := sha256.Sum256(body)
		info := meta.Bundles[name]
		if hex.EncodeToString(sum[:]) != info.SHA256 {
			t.Errorf("bundle %q content does not match metadata sha256", name)
		}
		if len(body) != info.Bytes {
			t.Errorf("bundle %q is %d bytes, metadata says %d", name, len(body), info.Bytes)
		}
	}
	if Has("does-not-exist") {
		t.Error("Has must be false for unknown workers")
	}
	if _, err := Script("does-not-exist"); err == nil {
		t.Error("Script must fail for unknown workers")
	}
}

func TestEmbeddedMigrations(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(migrations) < 40 {
		t.Errorf("expected the full upstream migration set, got %d files", len(migrations))
	}
	sql, ok := migrations["0001_initial_schema.sql"]
	if !ok {
		// The exact first filename is upstream's choice; require SOME 0001_.
		found := false
		for name := range migrations {
			if strings.HasPrefix(name, "0001_") {
				found, sql = true, migrations[name]
				break
			}
		}
		if !found {
			t.Fatalf("no 0001_* migration present")
		}
	}
	if !strings.Contains(strings.ToUpper(sql), "CREATE TABLE") {
		t.Errorf("first migration does not create tables")
	}
	for name := range migrations {
		if !strings.HasSuffix(name, ".sql") {
			t.Errorf("unexpected non-SQL migration file %q", name)
		}
	}
}
