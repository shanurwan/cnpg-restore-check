package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunNowAnnotation starts a run as soon as its value differs from the value
// recorded in status.lastManualTrigger.
const RunNowAnnotation = "restorecheck.shanurwan.github.io/run-now"

// RestoreCheckSpec defines how backups are restored and verified.
type RestoreCheckSpec struct {
	// Schedule is a five-field cron expression (UTC). A new check runs once
	// right away, then on this schedule. If empty, the check runs once after
	// creation and then only when triggered with the run-now annotation.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspend stops new scheduled runs. A run in progress is not interrupted.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Source is the backup to restore.
	Source SourceSpec `json:"source"`

	// Target selects the recovery target. The default is the latest
	// recoverable point (latest base backup plus all archived WAL).
	// +optional
	Target *TargetSpec `json:"target,omitempty"`

	// Cluster configures the temporary cluster the backup is restored into.
	// +optional
	Cluster ClusterTemplate `json:"cluster,omitempty"`

	// Checks run against the restored cluster once it is ready.
	// +optional
	Checks ChecksSpec `json:"checks,omitempty"`

	// Objectives turn measured recovery time and data age into pass/fail.
	// +optional
	Objectives *ObjectivesSpec `json:"objectives,omitempty"`

	// Timeout bounds a whole run, from creating the cluster to the last check.
	// +kubebuilder:default="2h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// KeepFailedClusters keeps the temporary cluster of a failed run for
	// debugging instead of deleting it. It is deleted when the next run starts.
	// +optional
	KeepFailedClusters bool `json:"keepFailedClusters,omitempty"`

	// HistoryLimit is the number of past runs kept in status.history.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	HistoryLimit *int32 `json:"historyLimit,omitempty"`
}

// SourceSpec points at a backup in an object store managed by plugin-barman-cloud.
type SourceSpec struct {
	// ObjectStoreName is a barmancloud.cnpg.io ObjectStore in the same namespace
	// as this RestoreCheck. Give it read-only credentials: a restore never
	// needs to write. It must not be an ObjectStore that a Cluster in this
	// namespace archives WAL to, unless allowSharedObjectStore is set.
	// +kubebuilder:validation:MinLength=1
	ObjectStoreName string `json:"objectStoreName"`

	// ServerName is the server name the source cluster archives under
	// (usually the source Cluster's name).
	// +kubebuilder:validation:MinLength=1
	ServerName string `json:"serverName"`

	// AllowSharedObjectStore allows an ObjectStore that a Cluster in this
	// namespace also uses for WAL archiving. Only set this if its credentials
	// are read-only or you accept the risk.
	// +optional
	AllowSharedObjectStore bool `json:"allowSharedObjectStore,omitempty"`
}

// TargetSpec selects a recovery target. At most one field may be set.
type TargetSpec struct {
	// BackupID restores a specific base backup.
	// +optional
	BackupID string `json:"backupID,omitempty"`

	// TargetTime restores to a point in time (RFC 3339).
	// +optional
	TargetTime string `json:"targetTime,omitempty"`

	// TargetImmediate stops recovery as soon as the base backup is consistent,
	// without replaying further WAL.
	// +optional
	TargetImmediate bool `json:"targetImmediate,omitempty"`
}

// ClusterTemplate configures the temporary CloudNativePG cluster.
type ClusterTemplate struct {
	// ImageName is the PostgreSQL image. It must be the same major version
	// as the source. The operator default is used if empty.
	// +optional
	ImageName string `json:"imageName,omitempty"`

	// StorageSize is the PVC size of the restored instance.
	// +kubebuilder:default="10Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClass for the restored instance's PVC.
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`

	// Resources for the restored instance.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Parameters are PostgreSQL parameters for the restored instance. Settings
	// such as max_connections must be at least the source's values, or
	// recovery stops.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ServiceAccountAnnotations are set on the restored cluster's service
	// account, for cloud identity such as IRSA (eks.amazonaws.com/role-arn) or
	// GKE Workload Identity (iam.gke.io/gcp-service-account). The service
	// account is named after the temporary cluster, <restorecheck>-<timestamp>,
	// so bind cloud roles with a wildcard or at namespace level.
	// +optional
	ServiceAccountAnnotations map[string]string `json:"serviceAccountAnnotations,omitempty"`

	// NodeSelector for the restored instance (for example, to run restores
	// on cheaper nodes).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// ChecksSpec lists the checks run against the restored cluster.
type ChecksSpec struct {
	// Amcheck verifies B-tree indexes (and, with heap, table pages) in every
	// database using the amcheck extension.
	// +optional
	Amcheck *AmcheckSpec `json:"amcheck,omitempty"`

	// SQL assertions. Each query must return exactly one row with one boolean
	// column that is true.
	// +optional
	SQL []SQLCheck `json:"sql,omitempty"`

	// Freshness measures how old the newest restored data is. The query must
	// return one timestamptz (for example, max(ts) from a heartbeat table).
	// The age is the run start time minus that timestamp.
	// +optional
	Freshness *FreshnessCheck `json:"freshness,omitempty"`
}

// AmcheckSpec configures the amcheck check.
type AmcheckSpec struct {
	// Heap also runs verify_heapam on every table, and checks that every heap
	// tuple has an index entry. Slower; reads every page.
	// +optional
	Heap bool `json:"heap,omitempty"`
}

// SQLCheck is a named SQL assertion.
type SQLCheck struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:default="postgres"
	// +optional
	Database string `json:"database,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`
}

// FreshnessCheck measures restored data age.
type FreshnessCheck struct {
	// +kubebuilder:default="postgres"
	// +optional
	Database string `json:"database,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`
}

// ObjectivesSpec are recovery objectives. A run that misses one fails.
type ObjectivesSpec struct {
	// RecoveryTime is the maximum time from creating the cluster to it being
	// ready (RTO).
	// +optional
	RecoveryTime *metav1.Duration `json:"recoveryTime,omitempty"`

	// DataAge is the maximum age of the newest restored data, as measured by
	// checks.freshness (RPO).
	// +optional
	DataAge *metav1.Duration `json:"dataAge,omitempty"`

	// SuccessAge is how old the last successful run may get before the
	// RestoreCheckStale alert fires. Exported as a metric; it does not fail runs.
	// +optional
	SuccessAge *metav1.Duration `json:"successAge,omitempty"`
}

// RunResult is the outcome of a run.
// +kubebuilder:validation:Enum=Succeeded;Failed
type RunResult string

const (
	ResultSucceeded RunResult = "Succeeded"
	ResultFailed    RunResult = "Failed"
)

// RunPhase is the phase of a run in progress.
type RunPhase string

const (
	PhaseRestoring RunPhase = "Restoring"
	PhaseChecking  RunPhase = "Checking"
)

// Failure reasons.
const (
	ReasonInvalidSpec     = "InvalidSpec"
	ReasonRecoveryFailed  = "RecoveryFailed"
	ReasonTimeout         = "Timeout"
	ReasonCheckFailed     = "CheckFailed"
	ReasonObjectiveMissed = "ObjectiveMissed"
	ReasonClusterLost     = "ClusterLost"
)

// CurrentRun tracks a run in progress.
type CurrentRun struct {
	ClusterName string      `json:"clusterName"`
	Phase       RunPhase    `json:"phase"`
	StartTime   metav1.Time `json:"startTime"`
	// ClusterUID is recorded once the cluster is first seen, to tell a cluster
	// that is not created yet from one that was deleted or replaced.
	// +optional
	ClusterUID string `json:"clusterUID,omitempty"`
	// +optional
	ReadyTime *metav1.Time `json:"readyTime,omitempty"`
}

// CheckResult is the result of one check.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// +optional
	Message string `json:"message,omitempty"`
}

// RunSummary records a finished run.
type RunSummary struct {
	ClusterName    string      `json:"clusterName"`
	StartTime      metav1.Time `json:"startTime"`
	CompletionTime metav1.Time `json:"completionTime"`
	Result         RunResult   `json:"result"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// RecoveryDuration is the time from creating the cluster to it being ready.
	// +optional
	RecoveryDuration *metav1.Duration `json:"recoveryDuration,omitempty"`
	// DataAge is the age of the newest restored data (checks.freshness).
	// +optional
	DataAge *metav1.Duration `json:"dataAge,omitempty"`
	// +optional
	Checks []CheckResult `json:"checks,omitempty"`
}

// RestoreCheckStatus is the observed state.
type RestoreCheckStatus struct {
	// +optional
	CurrentRun *CurrentRun `json:"currentRun,omitempty"`
	// +optional
	LastRun *RunSummary `json:"lastRun,omitempty"`
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// +optional
	NextRunTime *metav1.Time `json:"nextRunTime,omitempty"`
	// +optional
	LastManualTrigger string `json:"lastManualTrigger,omitempty"`
	// +optional
	History []RunSummary `json:"history,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rcheck
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.currentRun.phase`
// +kubebuilder:printcolumn:name="Last result",type=string,JSONPath=`.status.lastRun.result`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.lastRun.reason`
// +kubebuilder:printcolumn:name="Recovery",type=string,JSONPath=`.status.lastRun.recoveryDuration`
// +kubebuilder:printcolumn:name="Data age",type=string,JSONPath=`.status.lastRun.dataAge`
// +kubebuilder:printcolumn:name="Last success",type=date,JSONPath=`.status.lastSuccessTime`

// RestoreCheck periodically restores a CloudNativePG backup into a temporary
// cluster, verifies it, and records how long recovery took and how old the
// restored data is.
type RestoreCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreCheckSpec   `json:"spec,omitempty"`
	Status RestoreCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestoreCheckList contains a list of RestoreCheck.
type RestoreCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RestoreCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RestoreCheck{}, &RestoreCheckList{})
}
