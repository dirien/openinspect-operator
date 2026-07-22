package runtime

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	upstream "github.com/dirien/openinspect-operator/third_party/background-agents"
)

// The pins in versions.go are hand-mirrored from the upstream recipes; this
// test proves they match the exact synced revision, so bumping the upstream
// pin without re-auditing the toolchain fails the build instead of silently
// shipping a diverged runtime.

const (
	daytonaRecipe = "packages/daytona-infra/src/toolchain.py"
	modalRecipe   = "packages/modal-infra/src/images/base.py"
)

func recipeText(t *testing.T, path string) string {
	t.Helper()
	data, err := upstream.RecipeFile(path)
	if err != nil {
		t.Fatalf("read embedded recipe %s: %v", path, err)
	}
	return string(data)
}

func recipeConstant(t *testing.T, text, path, name string) string {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^%s\s*=\s*"([^"]+)"`, name))
	m := re.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("constant %s not found in %s", name, path)
	}
	return m[1]
}

func TestVersionPinsMatchUpstreamRecipes(t *testing.T) {
	for _, recipe := range []string{daytonaRecipe, modalRecipe} {
		text := recipeText(t, recipe)
		for name, want := range map[string]string{
			"OPENCODE_VERSION":      OpenCodeVersion,
			"CODE_SERVER_VERSION":   CodeServerVersion,
			"AGENT_BROWSER_VERSION": AgentBrowserVersion,
		} {
			if got := recipeConstant(t, text, recipe, name); got != want {
				t.Errorf("%s: %s is %q upstream but %q in versions.go", recipe, name, got, want)
			}
		}
		if !strings.Contains(text, "setup_"+NodeMajor+".x") {
			t.Errorf("%s does not install Node %s (setup_%s.x not found)", recipe, NodeMajor, NodeMajor)
		}
	}

	// ttyd is only pinned in the Modal recipe; the manifest carries it for
	// all providers.
	modal := recipeText(t, modalRecipe)
	if got := recipeConstant(t, modal, modalRecipe, "TTYD_VERSION"); got != TtydVersion {
		t.Errorf("%s: TTYD_VERSION is %q upstream but %q in versions.go", modalRecipe, got, TtydVersion)
	}
	if got := recipeConstant(t, modal, modalRecipe, "TTYD_SHA256"); got != TtydSHA256 {
		t.Errorf("%s: TTYD_SHA256 is %q upstream but %q in versions.go", modalRecipe, got, TtydSHA256)
	}

	daytona := recipeText(t, daytonaRecipe)
	if base := "python:" + PythonVersion + "-slim-bookworm"; !strings.Contains(daytona, base) {
		t.Errorf("%s does not use base image %q", daytonaRecipe, base)
	}
	for _, pkg := range aptPackages {
		if !strings.Contains(daytona, pkg) {
			t.Errorf("apt package %q is not part of the upstream Daytona recipe", pkg)
		}
	}
	for _, pkg := range pipPackages {
		if !strings.Contains(daytona, `"`+pkg+`"`) {
			t.Errorf("pip package %q is not part of the upstream Daytona recipe", pkg)
		}
	}
}
