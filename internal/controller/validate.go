package controller

import (
	"fmt"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// validateSpec catches cross-field references the CRD schema cannot express:
// bindings pointing at undefined resources would otherwise upload empty IDs
// or stay pending forever.
func validateSpec(oi *openinspectv1alpha1.OpenInspect) error {
	kv := map[string]bool{}
	for _, n := range oi.Spec.KVNamespaces {
		kv[n] = true
	}
	r2 := map[string]bool{}
	for _, n := range oi.Spec.R2Buckets {
		r2[n] = true
	}
	workers := map[string]bool{}
	for _, w := range oi.Spec.Workers {
		if workers[w.Name] {
			return fmt.Errorf("duplicate worker name %q", w.Name)
		}
		workers[w.Name] = true
	}
	if err := validateSandbox(oi.Spec.Sandbox, workers); err != nil {
		return err
	}
	for _, w := range oi.Spec.Workers {
		if len(w.Bindings.D1) > 0 && oi.Spec.D1 == nil {
			return fmt.Errorf("worker %q has a d1 binding but spec.d1 is not set", w.Name)
		}
		for _, b := range w.Bindings.KV {
			if !kv[b.Namespace] {
				return fmt.Errorf("worker %q kv binding %q references undefined namespace %q", w.Name, b.Binding, b.Namespace)
			}
		}
		for _, b := range w.Bindings.R2 {
			if !r2[b.Bucket] {
				return fmt.Errorf("worker %q r2 binding %q references undefined bucket %q", w.Name, b.Binding, b.Bucket)
			}
		}
		for _, b := range w.Bindings.Services {
			if !workers[b.Worker] {
				return fmt.Errorf("worker %q service binding %q references undefined worker %q", w.Name, b.Binding, b.Worker)
			}
		}
		if (w.Script.ConfigMapRef == nil) == (w.Script.URL == "") {
			return fmt.Errorf("worker %q script needs exactly one of configMapRef or url", w.Name)
		}
	}
	return nil
}
