package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	upstream "github.com/dirien/openinspect-operator/third_party/background-agents"
)

// File is one embedded sandbox-runtime file, addressed relative to the
// runtime directory (e.g. "entrypoint.py", "bin/oi-git-sign").
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// executableMode applies to files under bin/: go:embed drops file modes, so
// the executable bit is restored by convention. Upstream keeps exactly its
// executables there (bin/oi-git-sign).
const (
	executableMode fs.FileMode = 0o755
	regularMode    fs.FileMode = 0o644
)

// loadRuntimeFiles flattens the embedded sandbox_runtime tree into a
// deterministic, path-sorted list.
func loadRuntimeFiles() ([]File, error) {
	root, err := upstream.SandboxRuntimeFS()
	if err != nil {
		return nil, fmt.Errorf("sandbox runtime embed: %w", err)
	}
	var files []File
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		mode := regularMode
		if strings.HasPrefix(p, "bin/") {
			mode = executableMode
		}
		files = append(files, File{Path: path.Clean(p), Data: data, Mode: mode})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk sandbox runtime embed: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) == 0 {
		return nil, fmt.Errorf("sandbox runtime embed is empty")
	}
	return files, nil
}

// Materialize writes the runtime files under dir, restoring executable modes.
// Providers whose build APIs take a local directory (Daytona build contexts,
// docker build) consume this.
func (m *Manifest) Materialize(dir string) error {
	for _, f := range m.RuntimeFiles {
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		// World-readable on purpose: this tree becomes container image
		// content, where sandbox processes must traverse and read it.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // image content must be world-traversable
			return err
		}
		if err := os.WriteFile(dst, f.Data, f.Mode); err != nil {
			return err
		}
	}
	return nil
}
