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
	ConditionReady   = "Ready"
	ConditionD1      = "D1Ready"
	ConditionStorage = "StorageReady"
	ConditionWorkers = "WorkersReady"
	ConditionWeb     = "WebReady"
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

// WorkerBindings groups all binding kinds for one worker.
type WorkerBindings struct {
	// +optional
	D1 []D1Binding `json:"d1,omitempty"`
	// +optional
	KV []KVBinding `json:"kv,omitempty"`
	// +optional
	R2 []R2Binding `json:"r2,omitempty"`
	// +optional
	DurableObjects []DurableObjectBinding `json:"durableObjects,omitempty"`
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
	// +optional
	CompatibilityFlags []string `json:"compatibilityFlags,omitempty"`

	// +optional
	Bindings WorkerBindings `json:"bindings,omitempty"`

	// Vars become plain_text bindings (environment variables).
	// +optional
	Vars map[string]string `json:"vars,omitempty"`

	// Secrets become secret_text bindings, values read from Secrets.
	// +optional
	Secrets []WorkerSecret `json:"secrets,omitempty"`

	// Cron schedules for the worker (e.g. "* * * * *").
	// +optional
	Cron []string `json:"cron,omitempty"`
}

// ModalSandboxSpec configures the Modal sandbox provider. The Modal app
// itself is deployed with `modal deploy` (CLI-only upstream); the operator
// wires the control plane to it.
type ModalSandboxSpec struct {
	Workspace string `json:"workspace"`
	// +kubebuilder:default="main"
	// +optional
	Environment string `json:"environment,omitempty"`
	// +optional
	EnvironmentWebSuffix string `json:"environmentWebSuffix,omitempty"`

	TokenIDSecretRef     SecretKeyRef `json:"tokenIdSecretRef"`
	TokenSecretSecretRef SecretKeyRef `json:"tokenSecretSecretRef"`
	APISecretSecretRef   SecretKeyRef `json:"apiSecretSecretRef"`
}

// DaytonaSandboxSpec configures the Daytona sandbox provider.
type DaytonaSandboxSpec struct {
	// BaseSnapshot is the name of a prebuilt Daytona snapshot containing the
	// sandbox runtime.
	BaseSnapshot string `json:"baseSnapshot"`
	// APIURL of the Daytona API (e.g. https://app.daytona.io/api); the
	// upstream control plane requires it.
	APIURL string `json:"apiUrl"`
	// +optional
	Target string `json:"target,omitempty"`

	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`
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
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SecretEnv injects environment variables from Secret keys.
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
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`

	Cloudflare CloudflareSpec `json:"cloudflare"`

	// +optional
	D1 *D1Spec `json:"d1,omitempty"`

	// KVNamespaces are logical names; the Cloudflare title becomes
	// <namePrefix>-<name>-<deploymentName>.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +optional
	KVNamespaces []string `json:"kvNamespaces,omitempty"`

	// R2Buckets are logical names, expanded like KV namespaces.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9-]*$`
	// +optional
	R2Buckets []string `json:"r2Buckets,omitempty"`

	// +listType=map
	// +listMapKey=name
	// +optional
	Workers []WorkerSpec `json:"workers,omitempty"`

	// Sandbox wires one sandbox provider's configuration into the
	// control-plane worker.
	// +optional
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`

	// +optional
	Web *WebSpec `json:"web,omitempty"`

	// DeletionPolicy controls whether Cloudflare resources are deleted or
	// orphaned when the CR is deleted.
	// +kubebuilder:validation:Enum=Delete;Orphan
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
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

// ResourceName expands a logical name to the full Cloudflare resource name.
func (o *OpenInspect) ResourceName(logical string) string {
	prefix := o.Spec.NamePrefix
	if prefix == "" {
		prefix = "open-inspect"
	}
	return prefix + "-" + logical + "-" + o.Spec.DeploymentName
}
