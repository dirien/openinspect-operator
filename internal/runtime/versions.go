package runtime

// Toolchain pins for the provider-neutral sandbox runtime image. These mirror
// the pinned upstream recipes (packages/daytona-infra/src/toolchain.py and
// packages/modal-infra/src/images/base.py at third_party/background-agents);
// versions_parity_test.go fails the build when the pins drift from the synced
// upstream revision. Bump them only together with hack/sync-upstream.sh.
const (
	// PythonVersion selects the slim-bookworm base image tag.
	PythonVersion = "3.12"
	// NodeMajor selects the NodeSource setup script (Node.js LTS line).
	NodeMajor = "22"

	OpenCodeVersion     = "1.17.18"
	CodeServerVersion   = "4.109.5"
	AgentBrowserVersion = "0.21.2"

	TtydVersion = "1.7.7"
	TtydSHA256  = "8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55"
)

// aptPackages is the system package set every sandbox needs: build tooling,
// media encoding for browser videos, and the shared libraries headless
// Chromium links against. Order matters for the digest; keep upstream's.
var aptPackages = []string{
	"git", "curl", "build-essential", "ca-certificates", "gnupg",
	"openssh-client", "jq", "unzip",
	"libnss3", "libnspr4", "libatk1.0-0", "libatk-bridge2.0-0", "libcups2",
	"libdrm2", "libxkbcommon0", "libxcomposite1", "libxdamage1", "libxfixes3",
	"libxrandr2", "libgbm1", "libasound2", "libpango-1.0-0", "libcairo2",
	"ffmpeg",
}

// pipPackages are the Python dependencies of the sandbox runtime bridge.
var pipPackages = []string{
	"uv", "httpx", "websockets", "pydantic>=2.0", "PyJWT[crypto]",
}
