package cloudflare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/workers"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// ResolvedWorker carries everything needed to build one worker's bindings:
// the spec plus all name→ID/name resolutions done by the reconciler.
type ResolvedWorker struct {
	Spec         openinspectv1alpha1.WorkerSpec
	D1DatabaseID string
	// KVNamespaceIDs maps logical KV names to namespace IDs.
	KVNamespaceIDs map[string]string
	// R2BucketNames maps logical bucket names to full bucket names.
	R2BucketNames map[string]string
	// ServiceScriptNames maps logical worker names to full script names, but
	// only for workers that already exist on Cloudflare.
	ServiceScriptNames map[string]string
	// SecretValues maps binding names to resolved secret values.
	SecretValues map[string]string
}

// Bindings builds the upload metadata bindings. Service bindings whose target
// is missing from ServiceScriptNames are skipped and returned as pending.
func (w ResolvedWorker) Bindings() (bindings []workers.ScriptUpdateParamsMetadataBindingUnion, pending []string) {
	b := w.Spec.Bindings
	for _, d := range b.D1 {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindD1{
			Type:       cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindD1TypeD1),
			Name:       cf.F(d.Binding),
			DatabaseID: cf.F(w.D1DatabaseID),
		})
	}
	for _, k := range b.KV {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindKVNamespace{
			Type:        cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindKVNamespaceTypeKVNamespace),
			Name:        cf.F(k.Binding),
			NamespaceID: cf.F(w.KVNamespaceIDs[k.Namespace]),
		})
	}
	for _, r := range b.R2 {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindR2Bucket{
			Type:       cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindR2BucketTypeR2Bucket),
			Name:       cf.F(r.Binding),
			BucketName: cf.F(w.R2BucketNames[r.Bucket]),
		})
	}
	for _, d := range b.DurableObjects {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindDurableObjectNamespace{
			Type:      cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindDurableObjectNamespaceTypeDurableObjectNamespace),
			Name:      cf.F(d.Binding),
			ClassName: cf.F(d.ClassName),
		})
	}
	for _, s := range b.Services {
		target, ok := w.ServiceScriptNames[s.Worker]
		if !ok {
			pending = append(pending, s.Binding)
			continue
		}
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindService{
			Type:    cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindServiceTypeService),
			Name:    cf.F(s.Binding),
			Service: cf.F(target),
		})
	}
	for _, name := range sortedKeys(w.Spec.Vars) {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindPlainText{
			Type: cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindPlainTextTypePlainText),
			Name: cf.F(name),
			Text: cf.F(w.Spec.Vars[name]),
		})
	}
	for _, name := range sortedKeys(w.SecretValues) {
		bindings = append(bindings, workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindSecretText{
			Type: cf.F(workers.ScriptUpdateParamsMetadataBindingsWorkersBindingKindSecretTextTypeSecretText),
			Name: cf.F(name),
			Text: cf.F(w.SecretValues[name]),
		})
	}
	return bindings, pending
}

// Hash returns a digest over every input that goes into a version upload, so
// the reconciler only uploads when something changed.
func (w ResolvedWorker) Hash(script []byte) string {
	h := sha256.New()
	h.Write(script)
	// json.Marshal on the resolved struct covers bindings, IDs, vars, and
	// secret values; map key order is deterministic in encoding/json.
	blob, _ := json.Marshal(struct {
		Spec ResolvedWorker
	}{w})
	h.Write(blob)
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
