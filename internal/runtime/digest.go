package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	upstream "github.com/dirien/openinspect-operator/third_party/background-agents"
)

// digestSchemaVersion guards the canonical serialization: bump it whenever
// the digested structure changes shape, so equal-looking manifests from
// different operator versions can never alias.
const digestSchemaVersion = 1

func upstreamCommit() string { return upstream.Commit() }

type digestFile struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

type digestEnvelope struct {
	Schema      int               `json:"schema"`
	BaseImage   string            `json:"baseImage"`
	AptPackages []string          `json:"aptPackages"`
	PipPackages []string          `json:"pipPackages"`
	Steps       []Step            `json:"steps"`
	Env         map[string]string `json:"env"`
	RuntimeDir  string            `json:"runtimeDir"`
	WorkDir     string            `json:"workDir"`
	Entrypoint  []string          `json:"entrypoint"`
	Files       []digestFile      `json:"files"`
}

// Digest returns the deterministic content digest of the manifest as
// "sha256:<hex>". Identical digests mean identical provider artifacts;
// artifact names embed it, and a toolchain or runtime-file change produces
// exactly one new digest for every provider.
func (m *Manifest) Digest() (string, error) {
	files := make([]digestFile, len(m.RuntimeFiles))
	for i, f := range m.RuntimeFiles {
		sum := sha256.Sum256(f.Data)
		files[i] = digestFile{Path: f.Path, Mode: uint32(f.Mode), SHA256: hex.EncodeToString(sum[:])}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// encoding/json marshals maps with sorted keys, which makes the envelope
	// canonical without a custom serializer.
	payload, err := json.Marshal(digestEnvelope{
		Schema:      digestSchemaVersion,
		BaseImage:   m.BaseImage,
		AptPackages: m.AptPackages,
		PipPackages: m.PipPackages,
		Steps:       m.Steps,
		Env:         m.Env,
		RuntimeDir:  m.RuntimeDir,
		WorkDir:     m.WorkDir,
		Entrypoint:  m.Entrypoint,
		Files:       files,
	})
	if err != nil {
		return "", fmt.Errorf("marshal digest envelope: %w", err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ShortDigest returns the first 12 hex characters of the digest, the form
// embedded in artifact names and SANDBOX_VERSION.
func (m *Manifest) ShortDigest() (string, error) {
	d, err := m.Digest()
	if err != nil {
		return "", err
	}
	return d[len("sha256:") : len("sha256:")+12], nil
}

// SandboxVersion is the SANDBOX_VERSION environment value baked into images,
// replacing upstream's hand-bumped cache-buster strings with the content
// digest itself.
func (m *Manifest) SandboxVersion() (string, error) {
	short, err := m.ShortDigest()
	if err != nil {
		return "", err
	}
	return "oi-" + short, nil
}
