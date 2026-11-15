// Package metrics exports RestoreCheck results to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
)

const ns = "cnpg_restore_check"

var labels = []string{"namespace", "name"}

var (
	LastRunSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "last_run_success",
		Help: "1 if the last finished run succeeded, 0 if it failed.",
	}, labels)
	LastRunTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "last_run_timestamp_seconds",
		Help: "Completion time of the last finished run.",
	}, labels)
	LastSuccessTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "last_success_timestamp_seconds",
		Help: "Completion time of the last successful run.",
	}, labels)
	RecoveryDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "recovery_duration_seconds",
		Help: "Time from creating the temporary cluster to it being ready, in the last run that got that far (measured RTO).",
	}, labels)
	DataAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "data_age_seconds",
		Help: "Age of the newest restored data at run start, from checks.freshness, in the last run that measured it (measured RPO).",
	}, labels)
	RunInProgress = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "run_in_progress",
		Help: "1 while a run is in progress.",
	}, labels)
	RunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "runs_total",
		Help: "Finished runs by result and reason.",
	}, append(labels, "result", "reason"))
	ObjectiveRecoveryTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "objective_recovery_time_seconds",
		Help: "spec.objectives.recoveryTime, for alerting.",
	}, labels)
	ObjectiveDataAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "objective_data_age_seconds",
		Help: "spec.objectives.dataAge, for alerting.",
	}, labels)
	ObjectiveSuccessAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "objective_success_age_seconds",
		Help: "spec.objectives.successAge, for the RestoreCheckStale alert.",
	}, labels)
)

func init() {
	metrics.Registry.MustRegister(LastRunSuccess, LastRunTimestamp, LastSuccessTimestamp,
		RecoveryDuration, DataAge, RunInProgress, RunsTotal,
		ObjectiveRecoveryTime, ObjectiveDataAge, ObjectiveSuccessAge)
}

// Observe sets the gauges from a RestoreCheck's status and spec. It is
// idempotent, so it can be called on every reconcile, including after a
// controller restart.
func Observe(rc *v1alpha1.RestoreCheck) {
	l := prometheus.Labels{"namespace": rc.Namespace, "name": rc.Name}
	st := rc.Status

	if st.CurrentRun != nil {
		RunInProgress.With(l).Set(1)
	} else {
		RunInProgress.With(l).Set(0)
	}
	if lr := st.LastRun; lr != nil {
		if lr.Result == v1alpha1.ResultSucceeded {
			LastRunSuccess.With(l).Set(1)
		} else {
			LastRunSuccess.With(l).Set(0)
		}
		LastRunTimestamp.With(l).Set(float64(lr.CompletionTime.Unix()))
	}
	if st.LastSuccessTime != nil {
		LastSuccessTimestamp.With(l).Set(float64(st.LastSuccessTime.Unix()))
	}
	// Recovery time and data age come from the most recent run that measured them.
	for _, h := range st.History {
		if h.RecoveryDuration != nil {
			RecoveryDuration.With(l).Set(h.RecoveryDuration.Seconds())
			break
		}
	}
	for _, h := range st.History {
		if h.DataAge != nil {
			DataAge.With(l).Set(h.DataAge.Seconds())
			break
		}
	}

	setOrDelete := func(g *prometheus.GaugeVec, d *metav1.Duration) {
		if d != nil {
			g.With(l).Set(d.Seconds())
		} else {
			g.Delete(l)
		}
	}
	o := rc.Spec.Objectives
	if o == nil {
		o = &v1alpha1.ObjectivesSpec{}
	}
	setOrDelete(ObjectiveRecoveryTime, o.RecoveryTime)
	setOrDelete(ObjectiveDataAge, o.DataAge)
	setOrDelete(ObjectiveSuccessAge, o.SuccessAge)
}

// RecordRun increments the run counter for a finished run.
func RecordRun(rc *v1alpha1.RestoreCheck, s v1alpha1.RunSummary) {
	RunsTotal.WithLabelValues(rc.Namespace, rc.Name, string(s.Result), s.Reason).Inc()
}

// Forget deletes all series of a deleted RestoreCheck.
func Forget(namespace, name string) {
	l := prometheus.Labels{"namespace": namespace, "name": name}
	for _, g := range []*prometheus.GaugeVec{LastRunSuccess, LastRunTimestamp, LastSuccessTimestamp,
		RecoveryDuration, DataAge, RunInProgress, ObjectiveRecoveryTime, ObjectiveDataAge, ObjectiveSuccessAge} {
		g.Delete(l)
	}
	RunsTotal.DeletePartialMatch(l)
}
