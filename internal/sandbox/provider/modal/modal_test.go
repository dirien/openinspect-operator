package modal

import (
	"context"
	"errors"
	"strings"
	"testing"

	modal "github.com/modal-labs/modal-client/go"
)

func TestTerminateAllReturnsListFailure(t *testing.T) {
	want := errors.New("temporary modal outage")
	sandboxes := func(yield func(*modal.Sandbox, error) bool) {
		yield(nil, want)
	}

	_, err := terminateAll(context.Background(), sandboxes)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("error = %v, want wrapped list error", err)
	}
}

func TestTerminateAllAcceptsEmptyList(t *testing.T) {
	sandboxes := func(func(*modal.Sandbox, error) bool) {}

	n, err := terminateAll(context.Background(), sandboxes)
	if err != nil {
		t.Fatalf("empty sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated = %d, want 0", n)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{TokenSecret: "s"}); err == nil {
		t.Error("missing token id must be rejected")
	}
	if _, err := New(Config{TokenID: "i"}); err == nil {
		t.Error("missing token secret must be rejected")
	}
	p, err := New(Config{TokenID: "i", TokenSecret: "s", Workspace: "w"})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if p.cfg.Environment != DefaultEnvironment {
		t.Errorf("environment default = %q, want %q", p.cfg.Environment, DefaultEnvironment)
	}
}

func TestWebSuffixDerivation(t *testing.T) {
	cases := []struct {
		environment string
		url         string
		want        string
		wantErr     bool
	}{
		{environment: "main", url: "https://dirien--open-inspect-api-health.modal.run", want: ""},
		{environment: "staging", url: "https://dirien-staging--open-inspect-api-health.modal.run", want: "staging"},
		{environment: "envx", url: "https://dirien-env-x--open-inspect-api-health.modal.run", want: "env-x"},
		// A different workspace is a configuration error, not a suffix.
		{environment: "main", url: "https://other--open-inspect-api-health.modal.run", wantErr: true},
		// The other workspace merely PREFIXING ours must not pass either.
		{environment: "main", url: "https://dirienx--open-inspect-api-health.modal.run", wantErr: true},
		// The DEFAULT environment publishes without a suffix, so a dashed
		// slug means the workspace is wrong (e.g. real workspace
		// "dirien-corp"), never an environment suffix.
		{environment: "main", url: "https://dirien-corp--open-inspect-api-health.modal.run", wantErr: true},
		{environment: "main", url: "https://no-slug-separator.modal.run", wantErr: true},
	}
	for _, tc := range cases {
		p, err := New(Config{TokenID: "i", TokenSecret: "s", Workspace: "dirien", Environment: tc.environment})
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.webSuffix(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("webSuffix(%q, env %s) = %q, want error", tc.url, tc.environment, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("webSuffix(%q, env %s): %v", tc.url, tc.environment, err)
			continue
		}
		if got != tc.want {
			t.Errorf("webSuffix(%q, env %s) = %q, want %q", tc.url, tc.environment, got, tc.want)
		}
	}
}
