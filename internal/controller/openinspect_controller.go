/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
)

const (
	finalizer = "openinspect.dirien.dev/finalizer"

	// resyncInterval re-runs the idempotent ensure pass to pick up drift and
	// changes in referenced ConfigMaps/Secrets (which are not watched).
	resyncInterval = 5 * time.Minute
	// pendingInterval retries quickly while service bindings wait for their
	// target workers to exist.
	pendingInterval = 15 * time.Second

	maxScriptBytes = 25 << 20 // Cloudflare's own bundle limit
)

// OpenInspectReconciler reconciles a OpenInspect object.
type OpenInspectReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewCF is swappable in tests; defaults to cfclient.New.
	NewCF func(accountID, apiToken, baseURL string) *cfclient.Client
}

// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one OpenInspect deployment to its desired state: all
// Cloudflare resources (D1 + migrations, KV, R2, workers) plus the in-cluster
// web UI.
func (r *OpenInspectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	oi := &openinspectv1alpha1.OpenInspect{}
	if err := r.Get(ctx, req.NamespacedName, oi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion runs before anything that can fail (like credential lookup):
	// a CR must stay finalizable even when its token Secret is already gone.
	if !oi.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, oi)
	}

	if err := validateSpec(oi); err != nil {
		// Terminal until the spec changes: no backoff, generation predicate
		// retriggers on edit.
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSpec", err.Error())
		r.Recorder.Event(oi, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		oi.Status.ObservedGeneration = oi.Generation
		return ctrl.Result{}, r.persistStatus(ctx, oi)
	}

	cf, err := r.cloudflareClient(ctx, oi)
	if err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionReady, "CredentialsUnavailable", err)
	}

	if controllerutil.AddFinalizer(oi, finalizer) {
		if err := r.Update(ctx, oi); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureD1(ctx, oi, cf); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionD1, "D1Failed", err)
	}
	if err := r.ensureStorage(ctx, oi, cf); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionStorage, "StorageFailed", err)
	}
	pending, err := r.ensureWorkers(ctx, oi, cf)
	if err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionWorkers, "WorkersFailed", err)
	}
	if err := r.ensureWeb(ctx, oi); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionWeb, "WebFailed", err)
	}

	requeue := resyncInterval
	if pending > 0 {
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse,
			"ServiceBindingsPending", fmt.Sprintf("%d service bindings wait for their target worker", pending))
		requeue = pendingInterval
	} else {
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionTrue, "Reconciled", "all resources are up to date")
	}
	oi.Status.ObservedGeneration = oi.Generation
	if err := r.persistStatus(ctx, oi); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled", "pendingServiceBindings", pending)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *OpenInspectReconciler) cloudflareClient(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (*cfclient.Client, error) {
	token, err := r.secretValue(ctx, oi.Namespace, oi.Spec.Cloudflare.APITokenSecretRef)
	if err != nil {
		return nil, fmt.Errorf("cloudflare api token: %w", err)
	}
	newCF := r.NewCF
	if newCF == nil {
		newCF = cfclient.New
	}
	return newCF(oi.Spec.Cloudflare.AccountID, token, oi.Spec.Cloudflare.APIBaseURL), nil
}

func (r *OpenInspectReconciler) ensureD1(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) error {
	if oi.Spec.D1 == nil {
		return nil
	}
	id, err := cf.EnsureD1(ctx, oi.ResourceName("db"))
	if err != nil {
		return err
	}
	oi.Status.D1DatabaseID = id

	if ref := oi.Spec.D1.MigrationsConfigMapRef; ref != "" {
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: oi.Namespace, Name: ref}, cm); err != nil {
			return fmt.Errorf("migrations configmap: %w", err)
		}
		applied, err := cf.ApplyMigrations(ctx, id, cm.Data)
		oi.Status.AppliedMigrations = applied
		if err != nil {
			return err
		}
	}
	r.setCondition(oi, openinspectv1alpha1.ConditionD1, metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("database %s ready, %d migrations applied", id, oi.Status.AppliedMigrations))
	return nil
}

func (r *OpenInspectReconciler) ensureStorage(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) error {
	if oi.Status.KVNamespaceIDs == nil {
		oi.Status.KVNamespaceIDs = map[string]string{}
	}
	if oi.Status.R2Buckets == nil {
		oi.Status.R2Buckets = map[string]string{}
	}
	inSpecKV := map[string]bool{}
	for _, name := range oi.Spec.KVNamespaces {
		inSpecKV[name] = true
		id, err := cf.EnsureKV(ctx, oi.ResourceName(name))
		if err != nil {
			return err
		}
		oi.Status.KVNamespaceIDs[name] = id
	}
	inSpecR2 := map[string]bool{}
	for _, name := range oi.Spec.R2Buckets {
		inSpecR2[name] = true
		if err := cf.EnsureR2(ctx, oi.ResourceName(name)); err != nil {
			return err
		}
		oi.Status.R2Buckets[name] = oi.ResourceName(name)
	}
	// Prune resources removed from spec: status is the observed set.
	for name, id := range oi.Status.KVNamespaceIDs {
		if !inSpecKV[name] {
			if err := cf.DeleteKV(ctx, id); err != nil {
				return err
			}
			delete(oi.Status.KVNamespaceIDs, name)
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "KVNamespaceDeleted", "removed from spec: %s", name)
		}
	}
	for name, full := range oi.Status.R2Buckets {
		if !inSpecR2[name] {
			if err := cf.DeleteR2(ctx, full); err != nil {
				// A non-empty bucket cannot be deleted via the API: orphan it
				// with a warning rather than wedging the whole reconcile.
				r.Recorder.Eventf(oi, corev1.EventTypeWarning, "R2BucketOrphaned", "could not delete %s: %v", full, err)
			} else {
				r.Recorder.Eventf(oi, corev1.EventTypeNormal, "R2BucketDeleted", "removed from spec: %s", full)
			}
			delete(oi.Status.R2Buckets, name)
		}
	}
	r.setCondition(oi, openinspectv1alpha1.ConditionStorage, metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("%d KV namespaces, %d R2 buckets", len(oi.Spec.KVNamespaces), len(oi.Spec.R2Buckets)))
	return nil
}

// ensureWorkers uploads every worker whose inputs changed, then applies cron
// and subdomain settings. Service bindings to workers that do not exist yet
// are skipped this pass and reported as pending; the quick requeue converges
// them (including circular references) without a two-phase apply.
func (r *OpenInspectReconciler) ensureWorkers(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client) (pending int, err error) {
	if len(oi.Spec.Workers) == 0 && len(oi.Status.Workers) == 0 {
		return 0, nil
	}
	existing, err := cf.ListWorkerNames(ctx)
	if err != nil {
		return 0, err
	}
	if oi.Status.Workers == nil {
		oi.Status.Workers = map[string]openinspectv1alpha1.WorkerStatus{}
	}

	inSpec := map[string]bool{}
	for _, w := range oi.Spec.Workers {
		inSpec[w.Name] = true
		w = applySandbox(oi, w)
		scriptName := oi.ResourceName(w.Name)

		// Record the script name before any remote mutation so deletion can
		// always find what may have been created.
		st := oi.Status.Workers[w.Name]
		st.ScriptName = scriptName
		oi.Status.Workers[w.Name] = st

		script, err := r.scriptContent(ctx, oi.Namespace, w)
		if err != nil {
			return pending, fmt.Errorf("worker %q: %w", w.Name, err)
		}
		secrets := map[string]string{}
		for _, s := range w.Secrets {
			v, err := r.secretValue(ctx, oi.Namespace, s.SecretRef)
			if err != nil {
				return pending, fmt.Errorf("worker %q secret %q: %w", w.Name, s.Binding, err)
			}
			secrets[s.Binding] = v
		}
		liveServices := map[string]string{}
		for _, s := range w.Bindings.Services {
			if target := oi.ResourceName(s.Worker); existing[target] {
				liveServices[s.Worker] = target
			}
		}
		// Only what this worker references feeds its resolution (and hash) —
		// unrelated storage changes must not churn new versions.
		kvIDs := map[string]string{}
		for _, b := range w.Bindings.KV {
			kvIDs[b.Namespace] = oi.Status.KVNamespaceIDs[b.Namespace]
		}
		bucketNames := map[string]string{}
		for _, b := range w.Bindings.R2 {
			bucketNames[b.Bucket] = oi.ResourceName(b.Bucket)
		}
		d1ID := ""
		if len(w.Bindings.D1) > 0 {
			d1ID = oi.Status.D1DatabaseID
		}

		resolved := cfclient.ResolvedWorker{
			Spec:               w,
			D1DatabaseID:       d1ID,
			KVNamespaceIDs:     kvIDs,
			R2BucketNames:      bucketNames,
			ServiceScriptNames: liveServices,
			SecretValues:       secrets,
		}
		bindings, pendingBindings := resolved.Bindings()
		hash := resolved.Hash(script)

		st.ServiceBindingsPending = pendingBindings
		pending += len(pendingBindings)

		if st.AppliedHash != hash || !existing[scriptName] {
			// Durable Object classes must be introduced via migrations; diff
			// spec classes against what was already declared.
			declared := map[string]bool{}
			if existing[scriptName] {
				for _, c := range st.DOClasses {
					declared[c] = true
				}
			}
			var newClasses []string
			allClasses := make([]string, 0, len(w.Bindings.DurableObjects))
			for _, d := range w.Bindings.DurableObjects {
				allClasses = append(allClasses, d.ClassName)
				if !declared[d.ClassName] {
					newClasses = append(newClasses, d.ClassName)
				}
			}
			err := cf.UploadWorker(ctx, cfclient.WorkerUpload{
				ScriptName:         scriptName,
				Script:             script,
				CompatibilityDate:  w.CompatibilityDate,
				CompatibilityFlags: w.CompatibilityFlags,
				Bindings:           bindings,
				NewSqliteClasses:   newClasses,
			})
			if err != nil {
				oi.Status.Workers[w.Name] = st
				return pending, err
			}
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "WorkerUploaded", "uploaded new version of %s", scriptName)
			existing[scriptName] = true
			st.AppliedHash = hash
			st.DOClasses = allClasses
		}
		oi.Status.Workers[w.Name] = st

		// Unconditional so emptied spec.cron actually clears remote schedules.
		if err := cf.SetCron(ctx, scriptName, w.Cron); err != nil {
			return pending, err
		}
		if err := cf.EnableSubdomain(ctx, scriptName); err != nil {
			return pending, err
		}
	}

	// Prune workers removed from spec.
	for name, st := range oi.Status.Workers {
		if !inSpec[name] {
			if err := cf.DeleteWorker(ctx, st.ScriptName); err != nil {
				return pending, err
			}
			delete(oi.Status.Workers, name)
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "WorkerDeleted", "removed from spec: %s", st.ScriptName)
		}
	}

	if pending > 0 {
		r.setCondition(oi, openinspectv1alpha1.ConditionWorkers, metav1.ConditionFalse, "ServiceBindingsPending",
			fmt.Sprintf("%d service bindings pending", pending))
	} else {
		r.setCondition(oi, openinspectv1alpha1.ConditionWorkers, metav1.ConditionTrue, "Reconciled",
			fmt.Sprintf("%d workers deployed", len(oi.Spec.Workers)))
	}
	return pending, nil
}

func (r *OpenInspectReconciler) scriptContent(ctx context.Context, namespace string, w openinspectv1alpha1.WorkerSpec) ([]byte, error) {
	switch {
	case w.Script.ConfigMapRef != nil:
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: w.Script.ConfigMapRef.Name}, cm); err != nil {
			return nil, err
		}
		key := w.Script.ConfigMapRef.Key
		var raw []byte
		if data, ok := cm.Data[key]; ok {
			raw = []byte(data)
		} else if data, ok := cm.BinaryData[key]; ok {
			raw = data
		} else {
			return nil, fmt.Errorf("key %q not found in configmap %q", key, w.Script.ConfigMapRef.Name)
		}
		// Worker bundles regularly exceed the 1MiB ConfigMap limit; a .gz key
		// carries them compressed.
		if strings.HasSuffix(key, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return nil, fmt.Errorf("gunzip %q: %w", key, err)
			}
			defer func() { _ = zr.Close() }()
			body, err := io.ReadAll(io.LimitReader(zr, maxScriptBytes+1))
			if err != nil {
				return nil, fmt.Errorf("gunzip %q: %w", key, err)
			}
			if len(body) > maxScriptBytes {
				return nil, fmt.Errorf("script %q exceeds %d bytes", key, maxScriptBytes)
			}
			return body, nil
		}
		return raw, nil
	case w.Script.URL != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.Script.URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch script %s: status %d", w.Script.URL, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxScriptBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxScriptBytes {
			return nil, fmt.Errorf("script %s exceeds %d bytes", w.Script.URL, maxScriptBytes)
		}
		return body, nil
	default:
		return nil, fmt.Errorf("script needs configMapRef or url")
	}
}

func (r *OpenInspectReconciler) reconcileDelete(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(oi, finalizer) {
		return ctrl.Result{}, nil
	}
	if oi.Spec.DeletionPolicy != "Orphan" {
		// Credentials are only needed on this branch; Orphan must finalize
		// even when the token Secret is already gone.
		cf, err := r.cloudflareClient(ctx, oi)
		if err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "CredentialsUnavailable",
				"cannot delete Cloudflare resources: %v (set deletionPolicy: Orphan to skip)", err)
			return ctrl.Result{}, err
		}
		for _, st := range oi.Status.Workers {
			if st.ScriptName == "" {
				continue
			}
			if err := cf.DeleteWorker(ctx, st.ScriptName); err != nil {
				return ctrl.Result{}, err
			}
		}
		for _, id := range oi.Status.KVNamespaceIDs {
			if err := cf.DeleteKV(ctx, id); err != nil {
				return ctrl.Result{}, err
			}
		}
		if oi.Status.D1DatabaseID != "" {
			if err := cf.DeleteD1(ctx, oi.Status.D1DatabaseID); err != nil {
				return ctrl.Result{}, err
			}
		}
		for _, full := range oi.Status.R2Buckets {
			// ponytail: a non-empty bucket fails deletion; we surface the
			// error and keep retrying. Add object purge if that bites.
			if err := cf.DeleteR2(ctx, full); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(oi, finalizer)
	if err := r.Update(ctx, oi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// failed records the failing condition plus Ready=False and persists status.
// The error is returned for controller-runtime backoff.
func (r *OpenInspectReconciler) failed(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, condition, reason string, err error) (ctrl.Result, error) {
	r.setCondition(oi, condition, metav1.ConditionFalse, reason, err.Error())
	if condition != openinspectv1alpha1.ConditionReady {
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse, reason, err.Error())
	}
	r.Recorder.Event(oi, corev1.EventTypeWarning, reason, err.Error())
	if perr := r.persistStatus(ctx, oi); perr != nil {
		return ctrl.Result{}, perr
	}
	return ctrl.Result{}, err
}

func (r *OpenInspectReconciler) setCondition(oi *openinspectv1alpha1.OpenInspect, t string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&oi.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: oi.Generation,
	})
}

// persistStatus must land even during manager shutdown — a lost status write
// after a Cloudflare mutation would orphan IDs and re-create resources.
func (r *OpenInspectReconciler) persistStatus(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	status := oi.Status
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &openinspectv1alpha1.OpenInspect{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(oi), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status = status
		return r.Status().Update(ctx, latest)
	})
}

func (r *OpenInspectReconciler) secretValue(ctx context.Context, namespace string, ref openinspectv1alpha1.SecretKeyRef) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", err
	}
	v, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %q", ref.Key, ref.Name)
	}
	return string(v), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenInspectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// The new events API (GetEventRecorder) has an incompatible
		// interface; stay on the classic recorder until kubebuilder moves.
		r.Recorder = mgr.GetEventRecorderFor("openinspect-operator") //nolint:staticcheck
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Generation-gated so our own status writes never re-trigger; drift
		// and ConfigMap/Secret changes are covered by the periodic resync.
		For(&openinspectv1alpha1.OpenInspect{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("openinspect").
		Complete(r)
}
