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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types surfaced in status.conditions. Reasons on these conditions
// are machine API: renaming one is a breaking change.
const (
	ConditionReady      = "Ready"
	ConditionD1         = "D1Ready"
	ConditionStorage    = "StorageReady"
	ConditionWorkers    = "WorkersReady"
	ConditionWeb        = "WebReady"
	ConditionAppSecrets = "AppSecretsReady"
	// ConditionRuntime is True when the provider base artifact exists and
	// matches the release's runtime digest (or an external reference is
	// configured and found).
	ConditionRuntime = "RuntimeReady"
	// ConditionProviderApp is True when the provider-side application is
	// deployed and healthy (the upstream Modal app; trivially True for
	// providers without a deployed app).
	ConditionProviderApp = "ProviderAppReady"
)

// SecretKeyRef selects a key of a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ConfigMapKeyRef selects a key of a ConfigMap in the same namespace.
type ConfigMapKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// CloudflareSpec holds account credentials and API endpoint.
type CloudflareSpec struct {
	// AccountID is the Cloudflare account ID all resources are created under.
	AccountID string `json:"accountId"`

	// APITokenSecretRef points at the API token used for all Cloudflare calls.
	APITokenSecretRef SecretKeyRef `json:"apiTokenSecretRef"`

	// APIBaseURL overrides the Cloudflare API endpoint. Leave empty for
	// https://api.cloudflare.com/client/v4. Used for regional gateways and
	// for e2e tests against a mock API.
	// +optional
	APIBaseURL string `json:"apiBaseURL,omitempty"`
}

// D1Spec describes the single D1 database and its schema migrations.
type D1Spec struct {
	// MigrationsConfigMapRef names a ConfigMap whose keys are numbered
	// migration files (e.g. 0001_init.sql). They are applied in key order and
	// tracked in a _schema_migrations table by numeric prefix, matching the
	// upstream d1-migrate.sh semantics.
	// +optional
	MigrationsConfigMapRef string `json:"migrationsConfigMapRef,omitempty"`
}

// D1Binding attaches the database to a worker.
type D1Binding struct {
	// Binding is the variable name exposed to the worker (e.g. DB).
	Binding string `json:"binding"`
}

// KVBinding attaches a KV namespace to a worker.
type KVBinding struct {
	Binding string `json:"binding"`
	// Namespace is the logical name from spec.kvNamespaces.
	Namespace string `json:"namespace"`
}

// R2Binding attaches an R2 bucket to a worker.
type R2Binding struct {
	Binding string `json:"binding"`
	// Bucket is the logical name from spec.r2Buckets.
	Bucket string `json:"bucket"`
}

// DurableObjectBinding declares a Durable Object class the worker defines.
type DurableObjectBinding struct {
	Binding   string `json:"binding"`
	ClassName string `json:"className"`
}

// ServiceBinding attaches another worker managed by this CR.
type ServiceBinding struct {
	Binding string `json:"binding"`
	// Worker is the logical name from spec.workers. If the target worker does
	// not exist on Cloudflare yet, the binding is added on a later reconcile
	// pass once it does.
	Worker string `json:"worker"`
}

// WorkerBindings groups all binding kinds for one worker. The per-list caps
// stay within Cloudflare's own limit of 64 bindings per worker.
type WorkerBindings struct {
	// +kubebuilder:validation:MaxItems=64
	// +optional
	D1 []D1Binding `json:"d1,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +optional
	KV []KVBinding `json:"kv,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +optional
	R2 []R2Binding `json:"r2,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +optional
	DurableObjects []DurableObjectBinding `json:"durableObjects,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Services []ServiceBinding `json:"services,omitempty"`
}

// ScriptSource locates the built worker bundle (ESM, single module).
// Exactly one of configMapRef or url must be set.
// +kubebuilder:validation:XValidation:rule="has(self.configMapRef) != has(self.url)",message="exactly one of configMapRef or url must be set"
type ScriptSource struct {
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	URL string `json:"url,omitempty"`
}

// AppSecret maps one global secret key in the platform's secrets store to
// the Kubernetes Secret value that backs it. Keys are uppercase because the
// upstream store uppercases every key on write; admitting other spellings
// would make spec keys never match the store's listing.
type AppSecret struct {
	// +kubebuilder:validation:Pattern=`^[A-Z_][A-Z0-9_]*$`
	Key       string       `json:"key"`
	SecretRef SecretKeyRef `json:"secretRef"`
}

// WorkerSecret maps a Kubernetes Secret key to a secret binding / env var.
type WorkerSecret struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Binding   string       `json:"binding"`
	SecretRef SecretKeyRef `json:"secretRef"`
}

// WorkerSpec describes one Cloudflare Worker.
type WorkerSpec struct {
	// Name is the logical name (e.g. control-plane). The Cloudflare script
	// name becomes <namePrefix>-<name>-<deploymentName>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	Name string `json:"name"`

	Script ScriptSource `json:"script"`

	// +kubebuilder:default="2024-12-30"
	// +optional
	CompatibilityDate string `json:"compatibilityDate,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +optional
	CompatibilityFlags []string `json:"compatibilityFlags,omitempty"`

	// +optional
	Bindings WorkerBindings `json:"bindings,omitempty"`

	// Vars become plain_text bindings (environment variables).
	// +kubebuilder:validation:MaxProperties=128
	// +optional
	Vars map[string]string `json:"vars,omitempty"`

	// Secrets become secret_text bindings, values read from Secrets.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Secrets []WorkerSecret `json:"secrets,omitempty"`

	// Cron schedules for the worker (e.g. "* * * * *").
	// +kubebuilder:validation:MaxItems=10
	// +optional
	Cron []string `json:"cron,omitempty"`
}

// ModalSandboxSpec configures the Modal sandbox provider. The operator
// deploys the pinned upstream Modal app itself (modal-deploy Job) and
// discovers its endpoints; nothing endpoint-shaped is configured here.
type ModalSandboxSpec struct {
	Workspace string `json:"workspace"`
	// Environment is the Modal environment NAME the app is deployed in and
	// resolved from during cleanup.
	// +kubebuilder:default="main"
	// +optional
	Environment string `json:"environment,omitempty"`

	TokenIDSecretRef     SecretKeyRef `json:"tokenIdSecretRef"`
	TokenSecretSecretRef SecretKeyRef `json:"tokenSecretSecretRef"`
	APISecretSecretRef   SecretKeyRef `json:"apiSecretSecretRef"`
}

// DaytonaSandboxSpec configures the Daytona sandbox provider.
type DaytonaSandboxSpec struct {
	// BaseSnapshot names an externally owned, prebuilt Daytona snapshot to
	// use as-is. Empty means the operator builds and manages the base
	// snapshot from the release's bundled runtime.
	// +optional
	BaseSnapshot string `json:"baseSnapshot,omitempty"`
	// APIURL of the Daytona API (e.g. https://app.daytona.io/api); the
	// upstream control plane requires it, and deletion refuses to sweep
	// against an SDK fallback URL.
	// +kubebuilder:validation:MinLength=1
	APIURL string `json:"apiUrl"`
	// +optional
	Target string `json:"target,omitempty"`

	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`
}

// SandboxRuntimeSpec tunes the operator-managed provider base artifact. The
// managed build is the default; setting an explicit external reference
// (daytona.baseSnapshot) is the override that disables it.
type SandboxRuntimeSpec struct {
	// Image overrides the container base the managed artifact builds FROM
	// (e.g. a pinned ghcr.io/dirien/openinspect-sandbox-runtime tag). Empty
	// means the release's bundled runtime recipe, built from its base image.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`
	// UpdatePolicy controls when the managed artifact is rebuilt:
	// OnUpgrade rebuilds whenever the release's runtime digest changes;
	// Never keeps the current artifact (the reference still flips if one
	// with the current digest already exists).
	// +kubebuilder:validation:Enum=OnUpgrade;Never
	// +kubebuilder:default=OnUpgrade
	// +optional
	UpdatePolicy string `json:"updatePolicy,omitempty"`
	// DeletionPolicy controls whether managed artifacts are deleted with
	// the CR. Orphan retains them and records their references in status
	// and events before the finalizer is removed.
	// +kubebuilder:validation:Enum=Delete;Orphan
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// VercelSandboxSpec configures the Vercel sandbox provider. The control
// plane calls the Vercel Sandbox REST API directly.
type VercelSandboxSpec struct {
	ProjectID string `json:"projectId"`
	// +kubebuilder:default="node24"
	// +optional
	Runtime string `json:"runtime,omitempty"`
	// +optional
	TeamID string `json:"teamId,omitempty"`
	// +optional
	SnapshotExpirationMS int64 `json:"snapshotExpirationMs,omitempty"`
	// +optional
	APIBaseURL string `json:"apiBaseUrl,omitempty"`
	// BaseSnapshotID pins a prebuilt snapshot by ID.
	// +optional
	BaseSnapshotID string `json:"baseSnapshotId,omitempty"`
	// BaseSnapshotName pins a prebuilt snapshot by name.
	// +optional
	BaseSnapshotName string `json:"baseSnapshotName,omitempty"`

	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`
}

// OpenComputerSandboxSpec configures the OpenComputer sandbox provider.
type OpenComputerSandboxSpec struct {
	APIURL string `json:"apiUrl"`
	// Template is the OpenComputer template/snapshot to boot sandboxes from.
	Template string `json:"template"`

	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`
	// AnthropicAPIKeySecretRef is passed into sandboxes as ANTHROPIC_API_KEY
	// so the agent can call the model (upstream requires it for this
	// provider).
	// +optional
	AnthropicAPIKeySecretRef *SecretKeyRef `json:"anthropicApiKeySecretRef,omitempty"`
}

// SandboxSpec selects and configures exactly one sandbox provider. The
// operator injects the provider's vars and secrets into the control-plane
// worker; values the worker spec already sets explicitly win over injected
// ones.
type SandboxSpec struct {
	// +kubebuilder:validation:Enum=modal;daytona;vercel;opencomputer
	Provider string `json:"provider"`

	// ControlPlaneWorker names the worker (from spec.workers) that receives
	// the provider configuration.
	// +kubebuilder:default="control-plane"
	// +optional
	ControlPlaneWorker string `json:"controlPlaneWorker,omitempty"`

	// Runtime tunes the operator-managed provider base artifact.
	// +optional
	Runtime *SandboxRuntimeSpec `json:"runtime,omitempty"`

	// InactivityTimeoutMS stops idle sandboxes after this many milliseconds.
	// +optional
	InactivityTimeoutMS int64 `json:"inactivityTimeoutMs,omitempty"`

	// +optional
	Modal *ModalSandboxSpec `json:"modal,omitempty"`
	// +optional
	Daytona *DaytonaSandboxSpec `json:"daytona,omitempty"`
	// +optional
	Vercel *VercelSandboxSpec `json:"vercel,omitempty"`
	// +optional
	OpenComputer *OpenComputerSandboxSpec `json:"openComputer,omitempty"`
}

// WebSpec runs the web UI inside the cluster as a Deployment + Service.
type WebSpec struct {
	// Image is the container image of the web app.
	Image string `json:"image"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// +kubebuilder:default=3000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Env are plain environment variables.
	// +kubebuilder:validation:MaxProperties=128
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SecretEnv injects environment variables from Secret keys.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	SecretEnv []WorkerSecret `json:"secretEnv,omitempty"`
}

// OpenInspectSpec defines the desired state of one full deployment of the
// open-inspect platform: all Cloudflare resources plus the in-cluster web UI.
type OpenInspectSpec struct {
	// DeploymentName suffixes every Cloudflare resource name and must be
	// unique per Cloudflare account.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="deploymentName is immutable"
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	DeploymentName string `json:"deploymentName"`

	// +kubebuilder:default="open-inspect"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namePrefix is immutable"
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`

	// AdoptExisting lets this CR take ownership of Cloudflare resources that
	// already exist under its names (e.g. recreated after a deletionPolicy:
	// Orphan delete). Off by default so one CR can never silently capture
	// resources created by another deployment or tool.
	// +optional
	AdoptExisting bool `json:"adoptExisting,omitempty"`

	Cloudflare CloudflareSpec `json:"cloudflare"`

	// +optional
	D1 *D1Spec `json:"d1,omitempty"`

	// KVNamespaces are logical names; the Cloudflare title becomes
	// <namePrefix>-<name>-<deploymentName>.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +kubebuilder:validation:MaxItems=100
	// +optional
	KVNamespaces []string `json:"kvNamespaces,omitempty"`

	// R2Buckets are logical names, expanded like KV namespaces.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +kubebuilder:validation:MaxItems=100
	// +optional
	R2Buckets []string `json:"r2Buckets,omitempty"`

	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Workers []WorkerSpec `json:"workers,omitempty"`

	// Sandbox wires one sandbox provider's configuration into the
	// control-plane worker.
	// +optional
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`

	// +optional
	Web *WebSpec `json:"web,omitempty"`

	// AppSecrets seeds the platform's global secrets store (the
	// control-plane worker's /secrets API, injected into sandboxes at
	// creation) from Kubernetes Secrets after each worker deploy. Keys
	// removed from this list are deleted from the store; keys added out of
	// band (e.g. via the web UI) are left alone.
	// +listType=map
	// +listMapKey=key
	// +kubebuilder:validation:MaxItems=50
	// +optional
	AppSecrets []AppSecret `json:"appSecrets,omitempty"`

	// DeletionPolicy controls whether external resources are deleted or
	// orphaned when the CR is deleted. Orphan skips ALL external cleanup:
	// besides Cloudflare resources it also leaves running sandbox-provider
	// compute behind (Modal sandboxes and the deployed Modal app, Daytona
	// sandboxes) with nothing left that will ever stop it.
	// +kubebuilder:validation:Enum=Delete;Orphan
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// EmptyBucketsOnDelete purges all objects from R2 buckets before deleting
	// them (on CR deletion and when a bucket is removed from spec). Without
	// it, Cloudflare refuses to delete non-empty buckets: CR deletion blocks
	// and pruned buckets are orphaned with a warning event.
	// +optional
	EmptyBucketsOnDelete bool `json:"emptyBucketsOnDelete,omitempty"`

	// Suspended pauses reconciliation: no Cloudflare calls, no pruning, and
	// the web Deployment is scaled to zero. Nothing is deleted; setting it
	// back to false resumes where things left off. Deletion still finalizes
	// normally while suspended.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

// WorkerStatus records what is live for one worker.
type WorkerStatus struct {
	// ScriptName is the full Cloudflare script name.
	ScriptName string `json:"scriptName,omitempty"`
	// AppliedHash is the hash of the last uploaded version's inputs (script
	// content, bindings, vars, secret values). A new version is only uploaded
	// when it changes.
	AppliedHash string `json:"appliedHash,omitempty"`
	// ServiceBindingsPending lists service bindings waiting for their target
	// worker to exist.
	// +optional
	ServiceBindingsPending []string `json:"serviceBindingsPending,omitempty"`
	// DOClasses records Durable Object classes already declared to Cloudflare
	// via migrations; new spec classes are diffed against this on upload.
	// +optional
	DOClasses []string `json:"doClasses,omitempty"`
}

// OpenInspectStatus defines the observed state of OpenInspect.
type OpenInspectStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// D1DatabaseID is the UUID of the created database.
	// +optional
	D1DatabaseID string `json:"d1DatabaseId,omitempty"`

	// AppliedMigrations is the number of migrations recorded as applied.
	// +optional
	AppliedMigrations int `json:"appliedMigrations,omitempty"`

	// KVNamespaceIDs maps logical KV names to Cloudflare namespace IDs.
	// +optional
	KVNamespaceIDs map[string]string `json:"kvNamespaceIds,omitempty"`

	// R2Buckets maps logical bucket names to full Cloudflare bucket names, so
	// deletion works from observed state even after spec edits.
	// +optional
	R2Buckets map[string]string `json:"r2Buckets,omitempty"`

	// Workers maps logical worker names to their live state.
	// +optional
	Workers map[string]WorkerStatus `json:"workers,omitempty"`

	// AppSecrets records what was last seeded into the platform's global
	// secrets store, so removed keys can be deleted and unchanged values
	// skip the round-trip.
	// +optional
	AppSecrets *AppSecretsStatus `json:"appSecrets,omitempty"`

	// SandboxRuntime records the managed provider base artifact.
	// +optional
	SandboxRuntime *SandboxRuntimeStatus `json:"sandboxRuntime,omitempty"`

	// ModalApp records the operator-deployed upstream Modal app.
	// +optional
	ModalApp *ModalAppStatus `json:"modalApp,omitempty"`
}

// AppSecretsStatus records the last seeded global secrets.
type AppSecretsStatus struct {
	// AppliedHash is the hash of the last seeded key/value set.
	AppliedHash string `json:"appliedHash,omitempty"`
	// Keys are the store keys this CR manages; used to prune removed ones.
	// +optional
	Keys []string `json:"keys,omitempty"`
}

// Runtime artifact phases surfaced in status.sandboxRuntime.phase.
const (
	// RuntimePhasePending: a build is required but its Job has not
	// completed yet (includes waiting for a free build slot).
	RuntimePhasePending = "Pending"
	// RuntimePhaseBuilding: the build Job is running.
	RuntimePhaseBuilding = "Building"
	// RuntimePhaseReady: the artifact exists remotely and matches the
	// release's runtime digest.
	RuntimePhaseReady = "Ready"
	// RuntimePhaseFailed: the last build Job failed; the message on the
	// RuntimeReady condition carries the reason.
	RuntimePhaseFailed = "Failed"
	// RuntimePhaseExternal: an explicit artifact reference is configured;
	// the operator uses it as-is and never builds or deletes it.
	RuntimePhaseExternal = "External"
)

// SandboxRuntimeStatus records the managed provider base artifact. Phase
// describes convergence toward the release's runtime digest, while
// ArtifactName/ArtifactID/Digest/SourceRevision always describe the ACTIVE
// artifact the control plane is wired to — during an upgrade build the old
// reference keeps serving and only flips when the new artifact is ready.
type SandboxRuntimeStatus struct {
	Provider string `json:"provider,omitempty"`
	Phase    string `json:"phase,omitempty"`
	// SourceRevision is the pinned upstream commit the active artifact was
	// built from; Digest is its canonical runtime bundle digest (sha256:...).
	// +optional
	SourceRevision string `json:"sourceRevision,omitempty"`
	// +optional
	Digest string `json:"digest,omitempty"`
	// ArtifactName/ArtifactID identify the remote artifact (e.g. the
	// Daytona snapshot). The name embeds the digest, so a changed bundle
	// is a new artifact, never a mutation.
	// +optional
	ArtifactName string `json:"artifactName,omitempty"`
	// +optional
	ArtifactID string `json:"artifactId,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastBuildTime *metav1.Time `json:"lastBuildTime,omitempty"`
	// PreviousArtifacts are superseded managed artifacts retained for the
	// rollback grace period before garbage collection.
	// +kubebuilder:validation:MaxItems=10
	// +optional
	PreviousArtifacts []RetiredArtifact `json:"previousArtifacts,omitempty"`
}

// RetiredArtifact is a superseded managed artifact awaiting GC.
type RetiredArtifact struct {
	Name string `json:"name"`
	// +optional
	ID        string      `json:"id,omitempty"`
	RetiredAt metav1.Time `json:"retiredAt"`
}

// ModalAppStatus records the operator-deployed upstream Modal app.
type ModalAppStatus struct {
	// +optional
	AppID string `json:"appId,omitempty"`
	// HealthURL is the discovered api-health endpoint of the deployed app.
	// +optional
	HealthURL string `json:"healthUrl,omitempty"`
	// WebSuffix is the environment web suffix discovered from the deployed
	// endpoints ("" for the default environment); it is what the operator
	// injects as MODAL_ENVIRONMENT_WEB_SUFFIX.
	// +optional
	WebSuffix string `json:"webSuffix,omitempty"`
	// DeployedSourceRevision is the upstream commit the deploy Job ran.
	// +optional
	DeployedSourceRevision string `json:"deployedSourceRevision,omitempty"`
	// SecretsHash fingerprints the Modal secret inputs last converged
	// (HMAC-keyed; never reversible to values).
	// +optional
	SecretsHash string `json:"secretsHash,omitempty"`
	// +optional
	LastDeployTime *metav1.Time `json:"lastDeployTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=oi
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Migrations",type=integer,JSONPath=`.status.appliedMigrations`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OpenInspect is the Schema for the openinspects API.
type OpenInspect struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OpenInspectSpec   `json:"spec,omitempty"`
	Status OpenInspectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OpenInspectList contains a list of OpenInspect.
type OpenInspectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenInspect `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OpenInspect{}, &OpenInspectList{})
}

// namePrefix returns spec.namePrefix with its default applied.
func (o *OpenInspect) namePrefix() string {
	if o.Spec.NamePrefix != "" {
		return o.Spec.NamePrefix
	}
	return "open-inspect"
}

// ResourceName expands a logical name to the full Cloudflare resource name.
func (o *OpenInspect) ResourceName(logical string) string {
	return o.namePrefix() + "-" + logical + "-" + o.Spec.DeploymentName
}

// ClaimKey identifies the account-level naming domain this CR occupies. Two
// CRs with the same key would fight over the same Cloudflare resources, so
// the reconciler refuses to run all but the oldest claimant.
func (o *OpenInspect) ClaimKey() string {
	return o.Spec.Cloudflare.AccountID + "|" + o.namePrefix() + "|" + o.Spec.DeploymentName
}
