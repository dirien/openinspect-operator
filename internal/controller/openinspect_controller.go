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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
	cfclient "github.com/dirien/openinspect-operator/internal/cloudflare"
	"github.com/dirien/openinspect-operator/internal/sandbox/provider"
)

const (
	finalizer = "openinspect.dirien.dev/finalizer"

	// defaultResyncInterval re-runs the idempotent ensure pass to correct
	// drift. Referenced ConfigMap/Secret changes are picked up immediately by
	// watches; this is only the drift-correction floor.
	defaultResyncInterval = 5 * time.Minute
	// pendingInterval retries quickly while service bindings wait for their
	// target workers to exist.
	pendingInterval = 15 * time.Second
	// runtimePendingInterval polls remote artifact builds; Job completion
	// retriggers immediately via the Owns(Job) watch, so this only bounds
	// provider-side state changes the cluster cannot observe.
	runtimePendingInterval = 30 * time.Second

	maxScriptBytes = 25 << 20 // Cloudflare's own bundle limit
)

// OpenInspectReconciler reconciles a OpenInspect object.
type OpenInspectReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// ResyncInterval overrides how often a Ready object is re-reconciled for
	// drift correction (--resync-interval flag); zero means the default.
	ResyncInterval time.Duration

	// Live reads the primary object straight from the API server, bypassing
	// the informer cache (mgr.GetAPIReader()); nil falls back to Client.
	Live client.Reader

	// RuntimeBuilderImage is the image the runtime-build Job runs
	// (--runtime-builder-image flag / RELATED_IMAGE_RUNTIME_BUILDER env);
	// it carries the /runtime-builder binary from this repository.
	RuntimeBuilderImage string

	// NewCF is swappable in tests; defaults to cfclient.New.
	NewCF func(accountID, apiToken, baseURL string) *cfclient.Client
	// ModalCleanup is swappable in tests; defaults to cleanupModalApp.
	ModalCleanup func(context.Context, *openinspectv1alpha1.ModalSandboxSpec, string, string) (int, bool, error)
	// DaytonaCleanup is swappable in tests; defaults to sweepDaytonaSandboxes.
	DaytonaCleanup func(context.Context, *openinspectv1alpha1.DaytonaSandboxSpec, string, map[string]struct{}) (int, error)
	// AppSecretsURL overrides the global-secrets endpoint derivation in tests.
	AppSecretsURL func(*openinspectv1alpha1.OpenInspect) string
	// RuntimeInspect is swappable in tests; defaults to the provider adapter.
	RuntimeInspect func(context.Context, *openinspectv1alpha1.OpenInspect, provider.Artifact) (provider.ArtifactState, error)
}

func (r *OpenInspectReconciler) liveReader() client.Reader {
	if r.Live != nil {
		return r.Live
	}
	return r.Client
}

func (r *OpenInspectReconciler) resyncInterval() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return defaultResyncInterval
}

// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openinspect.dirien.dev,resources=openinspects/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one OpenInspect deployment to its desired state: all
// Cloudflare resources (D1 + migrations, KV, R2, workers) plus the in-cluster
// web UI.
func (r *OpenInspectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// The primary object is read LIVE, never from the informer cache: a
	// reconcile racing the cache can otherwise observe pre-mutation status
	// and, via its own persistStatus, wipe recorded resource IDs — after
	// which the ownership checks reject resources this CR itself created.
	oi := &openinspectv1alpha1.OpenInspect{}
	if err := r.liveReader().Get(ctx, req.NamespacedName, oi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion runs before anything that can fail (like credential lookup):
	// a CR must stay finalizable even when its token Secret is already gone.
	if !oi.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, oi)
	}

	if oi.Spec.Suspended {
		return r.reconcileSuspended(ctx, oi)
	}

	if err := validateSpec(oi); err != nil {
		// Terminal until the spec changes: no backoff, generation predicate
		// retriggers on edit.
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSpec", err.Error())
		r.Recorder.Event(oi, corev1.EventTypeWarning, "InvalidSpec", err.Error())
		oi.Status.ObservedGeneration = oi.Generation
		return ctrl.Result{}, r.persistStatus(ctx, oi)
	}

	// Before any remote call: refuse to run if an older CR already claims the
	// same account-level names, so two CRs can never capture each other's
	// Cloudflare resources.
	if owner, err := r.claimConflict(ctx, oi); err != nil {
		return ctrl.Result{}, err
	} else if owner != "" {
		err := fmt.Errorf("deploymentName %q on account %q is already claimed by OpenInspect %s; choose a different deploymentName",
			oi.Spec.DeploymentName, oi.Spec.Cloudflare.AccountID, owner)
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionReady, "DeploymentNameConflict", err)
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

	if err := timed("d1", func() error { return r.ensureD1(ctx, oi, cf) }); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionD1, "D1Failed", err)
	}
	if err := timed("storage", func() error { return r.ensureStorage(ctx, oi, cf) }); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionStorage, "StorageFailed", err)
	}
	// The managed base artifact converges before the workers: the worker
	// injection reads the artifact reference from status, and a later flip
	// (build finished) re-uploads the control-plane worker via the hash gate.
	var runtimePending bool
	err = timed("runtime", func() error {
		var rerr error
		runtimePending, rerr = r.ensureSandboxRuntime(ctx, oi)
		return rerr
	})
	if err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionRuntime, "RuntimeFailed", err)
	}
	var pending int
	err = timed("workers", func() error {
		var werr error
		pending, werr = r.ensureWorkers(ctx, oi, cf)
		return werr
	})
	if err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionWorkers, "WorkersFailed", err)
	}
	if err := timed("web", func() error { return r.ensureWeb(ctx, oi) }); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionWeb, "WebFailed", err)
	}
	if err := timed("appsecrets", func() error { return r.ensureAppSecrets(ctx, oi, cf) }); err != nil {
		return r.failed(ctx, oi, openinspectv1alpha1.ConditionAppSecrets, "AppSecretsFailed", err)
	}

	requeue := r.resyncInterval()
	switch {
	case pending > 0:
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse,
			"ServiceBindingsPending", fmt.Sprintf("%d service bindings wait for their target worker", pending))
		requeue = pendingInterval
	case runtimePending:
		// The Job watch retriggers on completion; this interval only bounds
		// remote-state polling while the provider builds.
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse,
			"RuntimeNotReady", "provider base artifact is not ready yet")
		requeue = runtimePendingInterval
	default:
		r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionTrue, "Reconciled", "all resources are up to date")
	}
	oi.Status.ObservedGeneration = oi.Generation
	if err := r.persistStatus(ctx, oi); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled", "pendingServiceBindings", pending, "runtimePending", runtimePending)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileSuspended parks the CR: the web Deployment is scaled to zero and
// no remote calls happen until spec.suspended is cleared, at which point the
// generation predicate retriggers a normal pass that restores everything.
func (r *OpenInspectReconciler) reconcileSuspended(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (ctrl.Result, error) {
	deploy := &appsv1.Deployment{}
	switch err := r.Get(ctx, types.NamespacedName{Name: oi.Name + "-web", Namespace: oi.Namespace}, deploy); {
	case client.IgnoreNotFound(err) != nil:
		return ctrl.Result{}, err
	// Only a deployment this CR owns is scaled: suspension runs before any
	// validation or claim check, so a same-named unrelated workload must
	// never be touched.
	case err == nil && metav1.IsControlledBy(deploy, oi) &&
		(deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 0):
		zero := int32(0)
		deploy.Spec.Replicas = &zero
		if err := r.Update(ctx, deploy); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(oi, corev1.EventTypeNormal, "Suspended", "web deployment scaled to zero")
	}
	r.setCondition(oi, openinspectv1alpha1.ConditionReady, metav1.ConditionFalse, "Suspended",
		"reconciliation suspended by spec.suspended")
	oi.Status.ObservedGeneration = oi.Generation
	return ctrl.Result{}, r.persistStatus(ctx, oi)
}

// deleteR2Bucket removes a bucket, purging its objects first when
// spec.emptyBucketsOnDelete is set.
func (r *OpenInspectReconciler) deleteR2Bucket(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client, full string) error {
	if oi.Spec.EmptyBucketsOnDelete {
		purged, err := cf.PurgeR2(ctx, full)
		if err != nil {
			return err
		}
		if purged > 0 {
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "R2BucketPurged", "deleted %d objects from %s", purged, full)
		}
	}
	return cf.DeleteR2(ctx, full)
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
	id, created, err := cf.EnsureD1(ctx, oi.ResourceName("db"))
	if err != nil {
		return err
	}
	// Covers both a fresh CR finding a foreign database and our recorded
	// database having been replaced by one we did not create.
	if !created && !oi.Spec.AdoptExisting && oi.Status.D1DatabaseID != id {
		return conflictf("d1 database %q already exists and is not owned by this OpenInspect (set spec.adoptExisting to take it over)", oi.ResourceName("db"))
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
	kvTitles := map[string]string{}
	if len(oi.Spec.KVNamespaces) > 0 {
		titles, err := cf.ListKVTitles(ctx)
		if err != nil {
			return err
		}
		kvTitles = titles
	}
	inSpecKV := map[string]bool{}
	for _, name := range oi.Spec.KVNamespaces {
		inSpecKV[name] = true
		full := oi.ResourceName(name)
		id, exists := kvTitles[full]
		switch {
		case !exists:
			created, err := cf.CreateKV(ctx, full)
			if err != nil {
				return err
			}
			id = created
		case !oi.Spec.AdoptExisting && oi.Status.KVNamespaceIDs[name] != id:
			return conflictf("kv namespace %q already exists and is not owned by this OpenInspect (set spec.adoptExisting to take it over)", full)
		}
		oi.Status.KVNamespaceIDs[name] = id
	}
	inSpecR2 := map[string]bool{}
	for _, name := range oi.Spec.R2Buckets {
		inSpecR2[name] = true
		created, err := cf.EnsureR2(ctx, oi.ResourceName(name))
		if err != nil {
			return err
		}
		if !created && !oi.Spec.AdoptExisting && oi.Status.R2Buckets[name] == "" {
			return conflictf("r2 bucket %q already exists and is not owned by this OpenInspect (set spec.adoptExisting to take it over)", oi.ResourceName(name))
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
			if err := r.deleteR2Bucket(ctx, oi, cf, full); err != nil {
				// A non-empty bucket cannot be deleted via the API (unless
				// spec.emptyBucketsOnDelete purges it): orphan it with a
				// warning rather than wedging the whole reconcile.
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
		// A script we never touched (no status entry) that already exists
		// remotely belongs to someone else.
		if st.ScriptName == "" && existing[scriptName] && !oi.Spec.AdoptExisting {
			return pending, conflictf("worker script %q already exists and is not owned by this OpenInspect (set spec.adoptExisting to take it over)", scriptName)
		}
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
			newClasses, allClasses := diffDOClasses(w, st, existing[scriptName])
			err := r.uploadWorkerVersion(ctx, oi, cf, cfclient.WorkerUpload{
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

// diffDOClasses returns the Durable Object classes to declare as new via
// migration (classes must be introduced exactly once) and the full set the
// worker defines.
func diffDOClasses(w openinspectv1alpha1.WorkerSpec, st openinspectv1alpha1.WorkerStatus, exists bool) (newClasses, allClasses []string) {
	declared := map[string]bool{}
	if exists {
		for _, c := range st.DOClasses {
			declared[c] = true
		}
	}
	allClasses = make([]string, 0, len(w.Bindings.DurableObjects))
	for _, d := range w.Bindings.DurableObjects {
		allClasses = append(allClasses, d.ClassName)
		if !declared[d.ClassName] {
			newClasses = append(newClasses, d.ClassName)
		}
	}
	return newClasses, allClasses
}

// uploadWorkerVersion uploads a version, adopting already-declared Durable
// Object classes when Cloudflare rejects a re-declaration (error 10074, e.g.
// after recorded state was lost).
func (r *OpenInspectReconciler) uploadWorkerVersion(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, cf *cfclient.Client, up cfclient.WorkerUpload) error {
	err := cf.UploadWorker(ctx, up)
	if err != nil && len(up.NewSqliteClasses) > 0 && cfclient.IsDOClassExists(err) {
		up.NewSqliteClasses = nil
		if err = cf.UploadWorker(ctx, up); err == nil {
			r.Recorder.Eventf(oi, corev1.EventTypeNormal, "DOClassesAdopted",
				"adopted existing durable object classes on %s", up.ScriptName)
		}
	}
	return err
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

func (r *OpenInspectReconciler) cleanupProviderSandboxes(
	ctx context.Context,
	oi *openinspectv1alpha1.OpenInspect,
	cf *cfclient.Client,
) error {
	sb := oi.Spec.Sandbox
	if sb == nil {
		return nil
	}
	switch sb.Provider {
	case providerModal:
		return r.cleanupModalProvider(ctx, oi, sb.Modal)
	case providerDaytona:
		return r.cleanupDaytonaProvider(ctx, oi, cf, sb.Daytona)
	default:
		return nil
	}
}

func (r *OpenInspectReconciler) cleanupModalProvider(
	ctx context.Context,
	oi *openinspectv1alpha1.OpenInspect,
	m *openinspectv1alpha1.ModalSandboxSpec,
) error {
	if m == nil {
		return nil
	}
	// The provider calls act on the whole Modal app. Another live CR pointing
	// at the same workspace and environment still depends on its sandbox pool.
	user, err := r.sharedModalApp(ctx, oi)
	if err != nil {
		return err
	}
	if user != "" {
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalCleanupSkipped",
			"modal app still in use by OpenInspect %s; leaving it running", user)
		return nil
	}

	tokenID, err := r.secretValue(ctx, oi.Namespace, m.TokenIDSecretRef)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "CredentialsUnavailable",
			"cannot clean up modal resources: %v (set deletionPolicy: Orphan to skip)", err)
		return err
	}
	tokenSecret, err := r.secretValue(ctx, oi.Namespace, m.TokenSecretSecretRef)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "CredentialsUnavailable",
			"cannot clean up modal resources: %v (set deletionPolicy: Orphan to skip)", err)
		return err
	}
	cleanup := cleanupModalApp
	if r.ModalCleanup != nil {
		cleanup = r.ModalCleanup
	}
	terminated, appFound, err := cleanup(ctx, m, tokenID, tokenSecret)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "ModalCleanupFailed", "%v", err)
		return err
	}
	if !appFound {
		// Not necessarily clean: the runtime addresses the app via web-suffix
		// URLs, so a wrong spec.environment NAME looks exactly like "no app".
		// Say what was (not) done instead of claiming a stop that never ran.
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "ModalAppNotFound",
			"no %q app in Modal environment %q; nothing cleaned — if the app lives elsewhere, verify spec.sandbox.modal.environment matches the deployed Modal environment name",
			modalAppName, modalEnvironment(m))
		return nil
	}
	if terminated > 0 {
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "ModalSandboxesTerminated",
			"terminated %d running sandboxes", terminated)
	}
	r.Recorder.Event(oi, corev1.EventTypeNormal, "ModalAppStopped", "modal app deployment stopped")
	return nil
}

func (r *OpenInspectReconciler) cleanupDaytonaProvider(
	ctx context.Context,
	oi *openinspectv1alpha1.OpenInspect,
	cf *cfclient.Client,
	d *openinspectv1alpha1.DaytonaSandboxSpec,
) error {
	if d == nil || oi.Status.D1DatabaseID == "" {
		return nil
	}
	sessionIDs, found, err := cf.D1SessionIDs(ctx, oi.Status.D1DatabaseID)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "DaytonaOwnershipReadFailed",
			"%v (set deletionPolicy: Orphan to skip)", err)
		return err
	}
	if !found {
		// Database or sessions table gone: no ownership evidence can ever
		// exist, so a sweep is impossible — degrade instead of wedging the
		// finalizer on the CR's own missing database.
		r.Recorder.Event(oi, corev1.EventTypeWarning, "DaytonaSweepSkipped",
			"no session ownership evidence in D1 (database or sessions table missing); leaving sandboxes to their auto-stop interval")
		return nil
	}
	owned := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		owned[id] = struct{}{}
	}
	apiKey, err := r.secretValue(ctx, oi.Namespace, d.APIKeySecretRef)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "CredentialsUnavailable",
			"cannot clean up daytona resources: %v (set deletionPolicy: Orphan to skip)", err)
		return err
	}
	cleanup := sweepDaytonaSandboxes
	if r.DaytonaCleanup != nil {
		cleanup = r.DaytonaCleanup
	}
	deleted, err := cleanup(ctx, d, apiKey, owned)
	if err != nil {
		r.Recorder.Eventf(oi, corev1.EventTypeWarning, "DaytonaCleanupFailed", "%v", err)
		return err
	}
	if deleted > 0 {
		r.Recorder.Eventf(oi, corev1.EventTypeNormal, "DaytonaSandboxesDeleted",
			"deleted %d owned sandboxes", deleted)
	}
	return nil
}

func (r *OpenInspectReconciler) reconcileDelete(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(oi, finalizer) {
		return ctrl.Result{}, nil
	}
	if oi.Spec.DeletionPolicy != "Orphan" {
		// Credentials are only needed on this branch; Orphan must finalize
		// even when the token Secret is already gone. Daytona ownership is
		// read from D1 before that database is deleted below.
		cf, err := r.cloudflareClient(ctx, oi)
		if err != nil {
			r.Recorder.Eventf(oi, corev1.EventTypeWarning, "CredentialsUnavailable",
				"cannot delete external resources: %v (set deletionPolicy: Orphan to skip)", err)
			return ctrl.Result{}, err
		}

		// Workers die first so no new sessions (and thus no new provider
		// sandboxes) can be created while the sweep below snapshots the D1
		// session index as its ownership evidence.
		for _, st := range oi.Status.Workers {
			if st.ScriptName == "" {
				continue
			}
			if err := cf.DeleteWorker(ctx, st.ScriptName); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Provider cleanup precedes the rest of the Cloudflare teardown
		// because Daytona reads the CR-owned D1 session index.
		if err := r.cleanupProviderSandboxes(ctx, oi, cf); err != nil {
			return ctrl.Result{}, err
		}

		for _, id := range oi.Status.KVNamespaceIDs {
			if err := cf.DeleteKV(ctx, id); err != nil {
				return ctrl.Result{}, err
			}
		}
		for _, full := range oi.Status.R2Buckets {
			// Without emptyBucketsOnDelete a non-empty bucket fails deletion;
			// the error surfaces and deletion retries until resolved.
			if err := r.deleteR2Bucket(ctx, oi, cf, full); err != nil {
				r.Recorder.Eventf(oi, corev1.EventTypeWarning, "R2BucketDeletionBlocked",
					"%v (set spec.emptyBucketsOnDelete to purge objects first)", err)
				return ctrl.Result{}, err
			}
		}
		// D1 goes LAST: it is the Daytona sweep's ownership evidence, and a
		// retry after any later failure must still be able to read it (the
		// sweep also tolerates a vanished database, but ordering keeps the
		// evidence alive for the normal path).
		if oi.Status.D1DatabaseID != "" {
			if err := cf.DeleteD1(ctx, oi.Status.D1DatabaseID); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(oi, finalizer)
	if err := r.Update(ctx, oi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	readyGauge.DeleteLabelValues(oi.Namespace, oi.Name)
	return ctrl.Result{}, nil
}

// conflictError marks adoption refusals so failed() reports them under the
// ResourceConflict reason instead of the area's generic failure reason.
type conflictError struct{ error }

func conflictf(format string, args ...any) error {
	return conflictError{fmt.Errorf(format, args...)}
}

// failed records the failing condition plus Ready=False and persists status.
// The error is returned for controller-runtime backoff.
func (r *OpenInspectReconciler) failed(ctx context.Context, oi *openinspectv1alpha1.OpenInspect, condition, reason string, err error) (ctrl.Result, error) {
	var conflict conflictError
	if errors.As(err, &conflict) {
		reason = "ResourceConflict"
	}
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
	if t == openinspectv1alpha1.ConditionReady {
		v := 0.0
		if status == metav1.ConditionTrue {
			v = 1
		}
		readyGauge.WithLabelValues(oi.Namespace, oi.Name).Set(v)
	}
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
		// Live read: rebasing the status write onto a cached object would
		// reintroduce the stale-clobber this function exists to prevent.
		if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(oi), latest); err != nil {
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
		r.Recorder = mgr.GetEventRecorderFor("openinspect-operator")
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Generation-gated so our own status writes never re-trigger.
		For(&openinspectv1alpha1.OpenInspect{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		// Metadata-only watches: any change to a referenced ConfigMap/Secret
		// re-reconciles immediately (the hash gate makes that cheap). The
		// manager client reads these objects uncached — see cmd/main.go.
		WatchesMetadata(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencing(referencesConfigMap)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesMetadata(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapReferencing(referencesSecret)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named("openinspect").
		Complete(r)
}
