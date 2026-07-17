package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errorReporter is the subset of testing.T/GinkgoT the fake API needs.
type errorReporter interface {
	Errorf(format string, args ...any)
}

// fakeSecretsAPI mimics the control-plane /secrets routes with the upstream
// auth scheme (Bearer timestamp.hmac) and response shapes.
type fakeSecretsAPI struct {
	secret string
	store  map[string]string
	puts   int
}

func (f *fakeSecretsAPI) handler(t errorReporter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ts, sig, ok := strings.Cut(auth, ".")
		if !ok {
			t.Errorf("malformed token %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mac := hmac.New(sha256.New, []byte(f.secret))
		mac.Write([]byte(ts))
		if hex.EncodeToString(mac.Sum(nil)) != sig {
			t.Errorf("bad signature for ts %s", ts)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			keys := []map[string]string{}
			for k := range f.store {
				keys = append(keys, map[string]string{"key": k})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"secrets": keys})
		case http.MethodPut:
			f.puts++
			var body struct {
				Secrets map[string]string `json:"secrets"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			maps.Copy(f.store, body.Secrets)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "updated"})
		case http.MethodDelete:
			key := strings.TrimPrefix(r.URL.Path, "/secrets/")
			delete(f.store, key)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func TestGlobalSecretsSyncRoundTrip(t *testing.T) {
	api := &fakeSecretsAPI{secret: "ics-secret", store: map[string]string{"FOREIGN": "keep"}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	endpoint := srv.URL + "/secrets"
	ctx := context.Background()

	// Seed two keys.
	if err := putGlobalSecrets(ctx, endpoint, "ics-secret", map[string]string{
		"ANTHROPIC_API_KEY": "sk-1", "OPENAI_API_KEY": "sk-2",
	}); err != nil {
		t.Fatal(err)
	}
	if api.store["ANTHROPIC_API_KEY"] != "sk-1" || api.store["OPENAI_API_KEY"] != "sk-2" {
		t.Fatalf("store not seeded: %v", api.store)
	}

	// Listing reflects all keys including foreign ones.
	keys, err := listGlobalSecretKeys(ctx, endpoint, "ics-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "FOREIGN"} {
		if !keys[k] {
			t.Errorf("missing key %s in %v", k, keys)
		}
	}

	// Pruning removes exactly the named key.
	if err := deleteGlobalSecret(ctx, endpoint, "ics-secret", "OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, exists := api.store["OPENAI_API_KEY"]; exists {
		t.Error("OPENAI_API_KEY not deleted")
	}
	if _, exists := api.store["FOREIGN"]; !exists {
		t.Error("foreign key must never be touched")
	}
}

func TestGlobalSecretsAuthFailure(t *testing.T) {
	api := &fakeSecretsAPI{secret: "right", store: map[string]string{}}
	srv := httptest.NewServer(api.handler(&testing.T{})) // swallow expected auth error
	defer srv.Close()

	if _, err := listGlobalSecretKeys(context.Background(), srv.URL+"/secrets", "wrong"); err == nil {
		t.Fatal("expected error on bad shared secret")
	}
}

func TestHashAppSecretsStable(t *testing.T) {
	a := hashAppSecrets(map[string]string{"A": "1", "B": "2"}, "ics")
	b := hashAppSecrets(map[string]string{"B": "2", "A": "1"}, "ics")
	if a != b {
		t.Error("hash must be order independent")
	}
	if a == hashAppSecrets(map[string]string{"A": "1", "B": "3"}, "ics") {
		t.Error("hash must change with values")
	}
	// Key/value boundaries must be unambiguous.
	if hashAppSecrets(map[string]string{"AB": "C"}, "ics") == hashAppSecrets(map[string]string{"A": "BC"}, "ics") {
		t.Error("hash must separate keys from values")
	}
	// The digest is keyed: without the key, the value cannot be brute-forced.
	if hashAppSecrets(map[string]string{"A": "1"}, "ics") == hashAppSecrets(map[string]string{"A": "1"}, "other") {
		t.Error("hash must depend on the HMAC key")
	}
}
