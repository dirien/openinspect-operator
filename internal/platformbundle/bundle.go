// Package platformbundle embeds the platform worker bundles built from the
// pinned upstream revision (hack/build-bundles.sh) and re-exposes the pinned
// D1 schema migrations. Users never create worker-bundles or d1-migrations
// ConfigMaps: the platform bundle version, web image, runtime digest, and
// operator version advance atomically with a release (EPIC1 section 7).
// ConfigMap and URL script sources remain as development-time overrides.
package platformbundle

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"sync"

	upstream "github.com/dirien/openinspect-operator/third_party/background-agents"
)

//go:embed *.js.gz metadata.json
var bundles embed.FS

// maxBundleBytes caps decompression; Cloudflare's own bundle limit is 25MiB.
const maxBundleBytes = 25 << 20

// Metadata describes the embedded bundle set.
type Metadata struct {
	UpstreamCommit string `json:"upstreamCommit"`
	Esbuild        string `json:"esbuild"`
	Bundles        map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	} `json:"bundles"`
}

var loadMetadata = sync.OnceValues(func() (Metadata, error) {
	raw, err := bundles.ReadFile("metadata.json")
	if err != nil {
		return Metadata{}, fmt.Errorf("platform bundle metadata: %w", err)
	}
	var meta Metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Metadata{}, fmt.Errorf("platform bundle metadata: %w", err)
	}
	return meta, nil
})

// Load returns the embedded bundle metadata.
func Load() (Metadata, error) { return loadMetadata() }

// Names lists the embedded worker bundle names, sorted.
func Names() ([]string, error) {
	meta, err := loadMetadata()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(meta.Bundles))
	for name := range meta.Bundles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Has reports whether a built-in bundle exists for the worker name.
func Has(name string) bool {
	meta, err := loadMetadata()
	if err != nil {
		return false
	}
	_, ok := meta.Bundles[name]
	return ok
}

var scriptCache sync.Map // name -> []byte (immutable, do not mutate)

// Script returns the uncompressed worker bundle for the given name. The
// returned slice is a shared cache entry — callers must not mutate it.
func Script(name string) ([]byte, error) {
	if cached, ok := scriptCache.Load(name); ok {
		return cached.([]byte), nil
	}
	body, err := decompressBundle(name)
	if err != nil {
		return nil, err
	}
	scriptCache.Store(name, body)
	return body, nil
}

func decompressBundle(name string) ([]byte, error) {
	raw, err := bundles.ReadFile(name + ".js.gz")
	if err != nil {
		return nil, fmt.Errorf("no built-in bundle %q: %w", name, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gunzip bundle %q: %w", name, err)
	}
	defer func() { _ = zr.Close() }()
	body, err := io.ReadAll(io.LimitReader(zr, maxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gunzip bundle %q: %w", name, err)
	}
	if len(body) > maxBundleBytes {
		return nil, fmt.Errorf("bundle %q exceeds %d bytes", name, maxBundleBytes)
	}
	return body, nil
}

var migrationsOnce = sync.OnceValues(loadMigrations)

// Migrations returns the pinned D1 schema migrations as filename -> SQL,
// the same shape a d1-migrations ConfigMap carries. The returned map is a
// shared cache entry — callers must not mutate it.
func Migrations() (map[string]string, error) {
	return migrationsOnce()
}

func loadMigrations() (map[string]string, error) {
	root, err := upstream.MigrationsFS()
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		files[p] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("embedded migrations: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("embedded migrations are empty")
	}
	return files, nil
}
