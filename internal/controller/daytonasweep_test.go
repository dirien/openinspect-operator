package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

func TestSweepDaytonaSandboxesDeletesOnlyOwnedSessions(t *testing.T) {
	t.Parallel()

	deleted := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sandbox":
			assertDaytonaFrameworkFilter(t, r.URL.Query())
			writeDaytonaJSON(t, w, http.StatusOK, map[string]any{
				"items": []any{
					daytonaSandboxPayload("owned", map[string]string{
						daytonaFrameworkLabel: daytonaFrameworkLabelValue,
						daytonaSessionIDLabel: "session-owned",
					}),
					daytonaSandboxPayload("foreign-session", map[string]string{
						daytonaFrameworkLabel: daytonaFrameworkLabelValue,
						daytonaSessionIDLabel: "session-foreign",
					}),
					daytonaSandboxPayload("foreign-framework", map[string]string{
						daytonaFrameworkLabel: "another-framework",
						daytonaSessionIDLabel: "session-owned",
					}),
				},
				"nextCursor": nil,
			})
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			writeDaytonaJSON(t, w, http.StatusOK, daytonaSandboxPayload("owned", nil))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	n, err := sweepDaytonaSandboxes(context.Background(), &openinspectv1alpha1.DaytonaSandboxSpec{
		APIURL: server.URL,
	}, "test-api-key", map[string]struct{}{"session-owned": {}})
	if err != nil {
		t.Fatalf("sweep daytona sandboxes: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d sandboxes, want 1", n)
	}
	if !slices.Equal(deleted, []string{"/sandbox/owned"}) {
		t.Fatalf("delete requests = %v, want [/sandbox/owned]", deleted)
	}
}

func TestSweepDaytonaSandboxesReturnsDeleteFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeDaytonaJSON(t, w, http.StatusOK, map[string]any{
				"items": []any{daytonaSandboxPayload("owned", map[string]string{
					daytonaFrameworkLabel: daytonaFrameworkLabelValue,
					daytonaSessionIDLabel: "session-owned",
				})},
				"nextCursor": nil,
			})
			return
		}
		http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := sweepDaytonaSandboxes(context.Background(), &openinspectv1alpha1.DaytonaSandboxSpec{
		APIURL: server.URL,
	}, "test-api-key", map[string]struct{}{"session-owned": {}})
	if err == nil {
		t.Fatal("delete failure must be returned so finalization retries")
	}
}

func assertDaytonaFrameworkFilter(t *testing.T, query url.Values) {
	t.Helper()
	var labels map[string]string
	if err := json.Unmarshal([]byte(query.Get("labels")), &labels); err != nil {
		t.Fatalf("decode labels filter: %v", err)
	}
	if labels[daytonaFrameworkLabel] != daytonaFrameworkLabelValue {
		t.Fatalf("labels filter = %v", labels)
	}
}

func daytonaSandboxPayload(id string, labels map[string]string) map[string]any {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[daytonatypes.CodeToolboxLanguageLabel] = string(daytonatypes.CodeLanguagePython)
	return map[string]any{
		"id":                  id,
		"organizationId":      "org-1",
		"name":                id,
		"user":                "daytona",
		"env":                 map[string]string{},
		"labels":              labels,
		"public":              false,
		"networkBlockAll":     false,
		"target":              "test",
		"cpu":                 1,
		"gpu":                 0,
		"memory":              2,
		"disk":                10,
		"state":               "started",
		"toolboxProxyUrl":     "http://toolbox.invalid",
		"autoArchiveInterval": 15,
		"autoDeleteInterval":  -1,
	}
}

func writeDaytonaJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
