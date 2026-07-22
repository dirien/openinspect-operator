package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	"github.com/dirien/openinspect-operator/internal/cloudflare"
)

// internalCallbackSecretBinding is the control-plane worker secret that
// authenticates service-to-service calls; appSecrets signs with it.
const internalCallbackSecretBinding = "INTERNAL_CALLBACK_SECRET"

// controlPlaneWorkerName returns the logical worker serving the platform API.
func controlPlaneWorkerName(oi *openinspectv1alpha1.OpenInspect) string {
	if sb := oi.Spec.Sandbox; sb != nil && sb.ControlPlaneWorker != "" {
		return sb.ControlPlaneWorker
	}
	return "control-plane"
}

// ensureAppSecrets converges the platform's global secrets store (served by
// the control-plane worker's /secrets API) with spec.appSecrets. Keys this
// CR seeded that later left the spec are deleted; keys added out of band
// (e.g. via the web UI) are never touched. A store wiped out of band is
// re-seeded because the remote key listing is consulted every pass.
func (r *OpenInspectReconciler) ensureAppSecrets(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cfc *cloudflare.Client) error {
	desired := oi.Spec.AppSecrets
	st := oi.Status.AppSecrets
	if len(desired) == 0 && (st == nil || len(st.Keys) == 0) {
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)
		return nil
	}

	values := make(map[string]string, len(desired))
	keys := make([]string, 0, len(desired))
	for _, s := range desired {
		v, err := r.secretValue(ctx, oi.Namespace, s.SecretRef)
		if err != nil {
			return fmt.Errorf("app secret %s: %w", s.Key, err)
		}
		values[s.Key] = v
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)

	ics, err := r.internalCallbackSecret(ctx, oi)
	if err != nil {
		if len(desired) > 0 {
			return err
		}
		// Teardown: appSecrets was emptied and the control-plane worker (or
		// its binding) removed in the same edit. The store died with the
		// worker, so there is nothing left to prune — record and move on
		// instead of wedging the CR on its own leftovers.
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "AppSecretsOrphaned",
			"cannot prune %d global secrets, skipping: %v", len(st.Keys), err)
		oi.Status.AppSecrets = nil
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)
		return nil
	}
	var endpoint string
	if r.AppSecretsURL != nil {
		endpoint = r.AppSecretsURL(oi)
	} else {
		subdomain, err := cfc.AccountSubdomain(ctx)
		if err != nil {
			return err
		}
		endpoint = fmt.Sprintf("https://%s.%s.workers.dev/secrets",
			oi.ResourceName(controlPlaneWorkerName(oi)), subdomain)
	}

	remote, err := listGlobalSecretKeys(ctx, endpoint, ics)
	if err != nil {
		return err
	}

	hash := hashAppSecrets(values, ics)
	missing := false
	for k := range values {
		if !remote[k] {
			missing = true
			break
		}
	}
	if len(values) > 0 && (missing || st == nil || st.AppliedHash != hash) {
		if err := putGlobalSecrets(ctx, endpoint, ics, values); err != nil {
			return err
		}
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "AppSecretsSeeded",
			"seeded %d global secrets into the control plane", len(values))
	}

	// Prune only keys this CR previously seeded and that left the spec.
	if st != nil {
		for _, k := range st.Keys {
			if _, still := values[k]; still || !remote[k] {
				continue
			}
			if err := deleteGlobalSecret(ctx, endpoint, ics, k); err != nil {
				return err
			}
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "AppSecretDeleted",
				"deleted global secret %s from the control plane", k)
		}
	}

	if len(values) == 0 {
		oi.Status.AppSecrets = nil
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionAppSecrets)
		return nil
	}
	oi.Status.AppSecrets = &openinspectv1alpha1.AppSecretsStatus{AppliedHash: hash, Keys: keys}
	r.setCondition(oi, openinspectv1alpha1.ConditionAppSecrets, metav1.ConditionTrue,
		"Reconciled", fmt.Sprintf("%d global secrets seeded", len(values)))
	return nil
}

// internalCallbackSecret resolves the control-plane worker's
// INTERNAL_CALLBACK_SECRET binding value, which authenticates /secrets calls.
func (r *OpenInspectReconciler) internalCallbackSecret(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (string, error) {
	return r.workerSecretValue(ctx, oi, internalCallbackSecretBinding)
}

// hashAppSecrets fingerprints the desired key/value set so unchanged values
// skip the PUT round-trip. The digest is keyed (HMAC) so the hash published
// in status cannot be dictionary-attacked to recover low-entropy values.
func hashAppSecrets(values map[string]string, key string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := hmac.New(sha256.New, []byte(key))
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(values[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// callInternalAPI performs one authenticated request against the
// control-plane's internal service API (Bearer timestamp.hmac-sha256-hex,
// same scheme the upstream web app uses).
func callInternalAPI(ctx context.Context, method, url, secret string, body []byte) ([]byte, error) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	token := ts + "." + hex.EncodeToString(mac.Sum(nil))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %.200s", method, url, resp.StatusCode, b)
	}
	return b, nil
}

// listGlobalSecretKeys returns the keys currently in the store.
func listGlobalSecretKeys(ctx context.Context, endpoint, secret string) (map[string]bool, error) {
	b, err := callInternalAPI(ctx, http.MethodGet, endpoint, secret, nil)
	if err != nil {
		return nil, fmt.Errorf("list global secrets: %w", err)
	}
	var out struct {
		Secrets []struct {
			Key string `json:"key"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("list global secrets: bad response: %w", err)
	}
	keys := make(map[string]bool, len(out.Secrets))
	for _, s := range out.Secrets {
		keys[s.Key] = true
	}
	return keys, nil
}

func putGlobalSecrets(ctx context.Context, endpoint, secret string, values map[string]string) error {
	body, err := json.Marshal(map[string]any{"secrets": values})
	if err != nil {
		return err
	}
	if _, err := callInternalAPI(ctx, http.MethodPut, endpoint, secret, body); err != nil {
		return fmt.Errorf("seed global secrets: %w", err)
	}
	return nil
}

func deleteGlobalSecret(ctx context.Context, endpoint, secret, key string) error {
	if _, err := callInternalAPI(ctx, http.MethodDelete, endpoint+"/"+key, secret, nil); err != nil {
		return fmt.Errorf("delete global secret %s: %w", key, err)
	}
	return nil
}
