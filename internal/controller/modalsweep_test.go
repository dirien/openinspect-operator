package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	modal "github.com/modal-labs/modal-client/go"
)

func TestTerminateModalSandboxesReturnsListFailure(t *testing.T) {
	want := errors.New("temporary modal outage")
	sandboxes := func(yield func(*modal.Sandbox, error) bool) {
		yield(nil, want)
	}

	_, err := terminateModalSandboxes(context.Background(), sandboxes)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("error = %v, want wrapped list error", err)
	}
}

func TestTerminateModalSandboxesAcceptsEmptyList(t *testing.T) {
	sandboxes := func(func(*modal.Sandbox, error) bool) {}

	n, err := terminateModalSandboxes(context.Background(), sandboxes)
	if err != nil {
		t.Fatalf("empty sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminated = %d, want 0", n)
	}
}
