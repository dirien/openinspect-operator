package runtime

import (
	"fmt"
	"sort"
	"strings"
)

// Dockerfile renders the manifest as a Dockerfile for the release
// sandbox-runtime image (ghcr.io/dirien/openinspect-sandbox-runtime). The
// build context is the repository root: the runtime tree is COPYed straight
// from the pinned third_party checkout, so `docker build -f
// Dockerfile.runtime .` needs no staging step.
//
// The image is linux/amd64 only: upstream pins amd64 artifacts (ttyd.x86_64,
// the code-server amd64 .deb, headless Chromium) and every supported
// provider runs amd64 sandboxes.
const runtimeContextDir = "third_party/background-agents/packages/sandbox-runtime/src/sandbox_runtime"

func (m *Manifest) Dockerfile() (string, error) {
	version, err := m.SandboxVersion()
	if err != nil {
		return "", err
	}
	digest, err := m.Digest()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Generated from internal/runtime — do not edit by hand.\n")
	b.WriteString("# Regenerate with: make generate-runtime\n")
	fmt.Fprintf(&b, "# Upstream commit: %s\n", m.UpstreamCommit)
	fmt.Fprintf(&b, "FROM %s\n", m.BaseImage)
	b.WriteString("LABEL org.opencontainers.image.title=\"openinspect-sandbox-runtime\" \\\n")
	b.WriteString("      org.opencontainers.image.description=\"Open-Inspect sandbox runtime base image (provider-neutral)\" \\\n")
	b.WriteString("      org.opencontainers.image.source=\"https://github.com/dirien/openinspect-operator\" \\\n")
	b.WriteString("      org.opencontainers.image.licenses=\"Apache-2.0 AND MIT\" \\\n")
	fmt.Fprintf(&b, "      dev.dirien.openinspect.upstream-commit=%q \\\n", m.UpstreamCommit)
	fmt.Fprintf(&b, "      dev.dirien.openinspect.runtime-digest=%q\n", digest)
	for _, s := range m.Steps {
		fmt.Fprintf(&b, "\n# %s\n", s.Name)
		fmt.Fprintf(&b, "RUN %s\n", strings.Join(s.Commands, " && \\\n    "))
	}

	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "ENV %s=%q\n", k, m.Env[k])
	}
	fmt.Fprintf(&b, "ENV SANDBOX_VERSION=%q\n", version)

	fmt.Fprintf(&b, "\nCOPY %s/ %s/\n", runtimeContextDir, m.RuntimeDir)
	fmt.Fprintf(&b, "WORKDIR %s\n", m.WorkDir)
	fmt.Fprintf(&b, "ENTRYPOINT [%s]\n", quoteJSONList(m.Entrypoint))
	return b.String(), nil
}

func quoteJSONList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
