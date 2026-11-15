// Package controller reconciles RestoreCheck objects.
package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
	"github.com/shanurwan/cnpg-restore-check/internal/checks"
	"github.com/shanurwan/cnpg-restore-check/internal/cnpg"
	"github.com/shanurwan/cnpg-restore-check/internal/metrics"
)

const (
	defaultTimeout      = 2 * time.Hour
	defaultHistoryLimit = 10
	pollInterval        = 15 * time.Second
	busyRetry           = time.Minute
	conditionLastRun    = "LastRunSucceeded"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Reconciler reconciles RestoreCheck objects.
type Reconciler struct {
	client.Client
	// APIReader reads Secrets, Jobs and ObjectStores without caching them.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	// RunChecks runs the checks against a restored cluster. Defaults to
	// checks.Run with a pgx connection.
	RunChecks func(ctx context.Context, t checks.Target, spec v1alpha1.ChecksSpec, runStart time.Time) checks.Result
	Now       func() time.Time
	// MaxConcurrentRuns limits runs in progress across all RestoreChecks; 0 is unlimited.
	MaxConcurrentRuns int
	// Host returns the address of a restored cluster's primary. Defaults to
	// the in-cluster -rw service.
	Host func(cluster, namespace string) string
}

// +kubebuilder:rbac:groups=restorecheck.shanurwan.github.io,resources=restorechecks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=restorecheck.shanurwan.github.io,resources=restorechecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=restorecheck.shanurwan.github.io,resources=restorechecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=barmancloud.cnpg.io,resources=objectstores,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one RestoreCheck: start a run when due, follow it, then
// record the result and clean up.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rc := &v1alpha1.RestoreCheck{}
	if err := r.Get(ctx, req.NamespacedName, rc); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.Forget(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !rc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	defer metrics.Observe(rc)

	if rc.Status.CurrentRun != nil {
		return r.progress(ctx, rc)
	}

	now := r.Now()
	due, next, manual, err := isDue(rc, now)
	if err != nil {
		return ctrl.Result{}, r.setInvalid(ctx, rc, err)
	}
	if !due {
		if next != nil && (rc.Status.NextRunTime == nil || !rc.Status.NextRunTime.Equal(next)) {
			rc.Status.NextRunTime = next
			if err := r.Status().Update(ctx, rc); err != nil {
				return ctrl.Result{}, err
			}
		}
		if next != nil {
			return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
		}
		return ctrl.Result{}, nil
	}

	if r.MaxConcurrentRuns > 0 {
		busy, err := r.runsInProgress(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if busy >= r.MaxConcurrentRuns {
			log.FromContext(ctx).Info("run due but the concurrency limit is reached", "inProgress", busy)
			return ctrl.Result{RequeueAfter: busyRetry}, nil
		}
	}
	return r.start(ctx, rc, now, manual)
}

// isDue reports whether a run should start now, and when the next scheduled
// run is. manual is the run-now annotation value when it triggered the run.
func isDue(rc *v1alpha1.RestoreCheck, now time.Time) (due bool, next *metav1.Time, manual string, err error) {
	if v := rc.Annotations[v1alpha1.RunNowAnnotation]; v != "" && v != rc.Status.LastManualTrigger {
		return true, nil, v, nil
	}
	if rc.Spec.Schedule == "" {
		return rc.Status.LastRun == nil && !rc.Spec.Suspend, nil, "", nil
	}
	sched, err := cronParser.Parse(rc.Spec.Schedule)
	if err != nil {
		return false, nil, "", fmt.Errorf("spec.schedule: %w", err)
	}
	if rc.Spec.Suspend {
		return false, nil, "", nil
	}
	// A new check runs once right away, so it proves itself without waiting
	// for its first slot; after that it follows the schedule.
	if rc.Status.LastRun == nil {
		return true, nil, "", nil
	}
	n := metav1.NewTime(sched.Next(rc.Status.LastRun.StartTime.UTC()))
	return !now.Before(n.Time), &n, "", nil
}

func (r *Reconciler) runsInProgress(ctx context.Context) (int, error) {
	var list v1alpha1.RestoreCheckList
	if err := r.List(ctx, &list); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range list.Items {
		if it.Status.CurrentRun != nil {
			n++
		}
	}
	return n, nil
}

// validate checks the spec and the source before a run creates anything.
func (r *Reconciler) validate(ctx context.Context, rc *v1alpha1.RestoreCheck) error {
	o := rc.Spec.Objectives
	if o != nil && o.DataAge != nil && rc.Spec.Checks.Freshness == nil {
		return fmt.Errorf("spec.objectives.dataAge needs spec.checks.freshness to measure data age")
	}
	if _, err := cnpg.BuildCluster(rc, "validate"); err != nil {
		return err
	}
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(cnpg.ObjectStoreGVK)
	key := client.ObjectKey{Namespace: rc.Namespace, Name: rc.Spec.Source.ObjectStoreName}
	if err := r.APIReader.Get(ctx, key, store); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ObjectStore %q not found in namespace %q", key.Name, key.Namespace)
		}
		return err
	}
	if !rc.Spec.Source.AllowSharedObjectStore {
		clusters := &unstructured.UnstructuredList{}
		clusters.SetGroupVersionKind(cnpg.ClusterListGVK)
		if err := r.List(ctx, clusters, client.InNamespace(rc.Namespace)); err != nil {
			return err
		}
		if users := cnpg.ObjectStoreWriters(clusters.Items, key.Name); len(users) > 0 {
			return fmt.Errorf("ObjectStore %q is used for archiving by Cluster(s) %s; "+
				"create a separate ObjectStore with read-only credentials for restore checks, "+
				"or set spec.source.allowSharedObjectStore", key.Name, strings.Join(users, ", "))
		}
	}
	return nil
}

// start begins a run. It records the run in status before creating anything:
// the status update fails with a conflict if this reconcile works from a stale
// cache, so a stale view can never delete or create clusters.
func (r *Reconciler) start(ctx context.Context, rc *v1alpha1.RestoreCheck, now time.Time, manual string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if manual != "" {
		rc.Status.LastManualTrigger = manual
	}

	if err := r.validate(ctx, rc); err != nil {
		t := metav1.NewTime(now)
		return r.finish(ctx, rc, v1alpha1.RunSummary{
			StartTime: t, CompletionTime: t, Result: v1alpha1.ResultFailed,
			Reason: v1alpha1.ReasonInvalidSpec, Message: err.Error(),
		})
	}

	name := clusterName(rc.Name, now)
	rc.Status.CurrentRun = &v1alpha1.CurrentRun{
		ClusterName: name, Phase: v1alpha1.PhaseRestoring, StartTime: metav1.NewTime(now),
	}
	rc.Status.NextRunTime = nil
	if err := r.Status().Update(ctx, rc); err != nil {
		return ctrl.Result{}, err
	}

	// Remove clusters kept from earlier failed runs (never the current one).
	if err := r.deleteClusters(ctx, rc, name); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.createCluster(ctx, rc, name); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("restore check started", "cluster", name)
	r.Recorder.Eventf(rc, corev1.EventTypeNormal, "RunStarted", "Restoring into Cluster %s", name)
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *Reconciler) createCluster(ctx context.Context, rc *v1alpha1.RestoreCheck, name string) error {
	cl, err := cnpg.BuildCluster(rc, name)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(rc, cl, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cl); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cluster %s: %w", name, err)
	}
	return nil
}

// clusterName is <restorecheck>-<UTC timestamp>, kept short enough for the
// names CloudNativePG derives from it (pods, services, PVCs).
func clusterName(rc string, now time.Time) string {
	if len(rc) > 30 {
		rc = strings.TrimRight(rc[:30], "-.")
	}
	return fmt.Sprintf("%s-%s", rc, now.UTC().Format("20060102-150405"))
}

func (r *Reconciler) progress(ctx context.Context, rc *v1alpha1.RestoreCheck) (ctrl.Result, error) {
	run := rc.Status.CurrentRun
	now := r.Now()
	fail := func(reason, msg string) (ctrl.Result, error) {
		s := r.summary(rc, now)
		s.Result, s.Reason, s.Message = v1alpha1.ResultFailed, reason, msg
		return r.finish(ctx, rc, s)
	}

	cl := cnpg.NewCluster()
	if err := r.Get(ctx, client.ObjectKey{Namespace: rc.Namespace, Name: run.ClusterName}, cl); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if run.ClusterUID != "" {
			return fail(v1alpha1.ReasonClusterLost, fmt.Sprintf("Cluster %s disappeared during the run", run.ClusterName))
		}
		// Not seen yet: either the create after the status update did not
		// happen (crash, error) or the cache has not caught up. Create is
		// idempotent.
		if err := r.createCluster(ctx, rc, run.ClusterName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	uid := string(cl.GetUID())
	if uid == "" {
		uid = "unknown"
	}
	if run.ClusterUID == "" {
		run.ClusterUID = uid
		if err := r.Status().Update(ctx, rc); err != nil {
			return ctrl.Result{}, err
		}
		// Update decodes the response into rc, replacing CurrentRun.
		run = rc.Status.CurrentRun
	} else if uid != run.ClusterUID {
		return fail(v1alpha1.ReasonClusterLost, fmt.Sprintf("Cluster %s was replaced during the run", run.ClusterName))
	}

	timeout := defaultTimeout
	if rc.Spec.Timeout != nil {
		timeout = rc.Spec.Timeout.Duration
	}
	deadline := run.StartTime.Add(timeout)
	if now.After(deadline) {
		phase, reason := cnpg.Phase(cl)
		return fail(v1alpha1.ReasonTimeout, fmt.Sprintf("run exceeded %s in phase %s (cluster phase %q: %s)", timeout, run.Phase, phase, reason))
	}

	switch run.Phase {
	case v1alpha1.PhaseRestoring:
		if cnpg.IsReady(cl) {
			t := metav1.NewTime(now)
			run.ReadyTime = &t
			run.Phase = v1alpha1.PhaseChecking
			if err := r.Status().Update(ctx, rc); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		if msg, err := r.failedRecoveryJob(ctx, rc.Namespace, run.ClusterName); err != nil {
			return ctrl.Result{}, err
		} else if msg != "" {
			return fail(v1alpha1.ReasonRecoveryFailed, msg)
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case v1alpha1.PhaseChecking:
		target, err := r.target(ctx, rc.Namespace, run.ClusterName)
		if err != nil {
			return ctrl.Result{RequeueAfter: pollInterval}, err
		}
		cctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		res := r.RunChecks(cctx, target, rc.Spec.Checks, run.StartTime.Time)
		s := r.summary(rc, r.Now())
		s.Checks = res.Checks
		if res.DataAge != nil {
			s.DataAge = &metav1.Duration{Duration: res.DataAge.Round(time.Second)}
		}
		evaluate(rc, &s, res.Passed())
		return r.finish(ctx, rc, s)
	}
	return fail(v1alpha1.ReasonInvalidSpec, fmt.Sprintf("unknown run phase %q", run.Phase))
}

// evaluate sets the result from check results and objectives.
func evaluate(rc *v1alpha1.RestoreCheck, s *v1alpha1.RunSummary, checksPassed bool) {
	if !checksPassed {
		var failed []string
		for _, c := range s.Checks {
			if !c.Passed {
				failed = append(failed, fmt.Sprintf("%s: %s", c.Name, c.Message))
			}
		}
		s.Result, s.Reason = v1alpha1.ResultFailed, v1alpha1.ReasonCheckFailed
		s.Message = strings.Join(failed, "; ")
		return
	}
	var missed []string
	if o := rc.Spec.Objectives; o != nil {
		if o.RecoveryTime != nil && s.RecoveryDuration != nil && s.RecoveryDuration.Duration > o.RecoveryTime.Duration {
			missed = append(missed, fmt.Sprintf("recovery took %s, objective %s", s.RecoveryDuration.Duration, o.RecoveryTime.Duration))
		}
		if o.DataAge != nil && s.DataAge != nil && s.DataAge.Duration > o.DataAge.Duration {
			missed = append(missed, fmt.Sprintf("newest restored data is %s old, objective %s", s.DataAge.Duration, o.DataAge.Duration))
		}
	}
	if len(missed) > 0 {
		s.Result, s.Reason, s.Message = v1alpha1.ResultFailed, v1alpha1.ReasonObjectiveMissed, strings.Join(missed, "; ")
		return
	}
	s.Result = v1alpha1.ResultSucceeded
}

func (r *Reconciler) summary(rc *v1alpha1.RestoreCheck, now time.Time) v1alpha1.RunSummary {
	run := rc.Status.CurrentRun
	s := v1alpha1.RunSummary{ClusterName: run.ClusterName, StartTime: run.StartTime, CompletionTime: metav1.NewTime(now)}
	if run.ReadyTime != nil {
		s.RecoveryDuration = &metav1.Duration{Duration: run.ReadyTime.Sub(run.StartTime.Time).Round(time.Second)}
	}
	return s
}

// failedRecoveryJob returns a message if the cluster's recovery job failed.
func (r *Reconciler) failedRecoveryJob(ctx context.Context, namespace, cluster string) (string, error) {
	var jobs batchv1.JobList
	if err := r.APIReader.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{"cnpg.io/cluster": cluster}); err != nil {
		return "", err
	}
	for _, j := range jobs.Items {
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return fmt.Sprintf("job %s failed (%s: %s); inspect it with: kubectl logs -n %s job/%s",
					j.Name, c.Reason, c.Message, namespace, j.Name), nil
			}
		}
	}
	return "", nil
}

func (r *Reconciler) target(ctx context.Context, namespace, cluster string) (checks.Target, error) {
	var sec corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster + "-superuser"}, &sec); err != nil {
		return checks.Target{}, fmt.Errorf("superuser secret: %w", err)
	}
	host := fmt.Sprintf("%s-rw.%s.svc", cluster, namespace)
	if r.Host != nil {
		host = r.Host(cluster, namespace)
	}
	user := string(sec.Data["username"])
	if user == "" {
		user = "postgres"
	}
	return checks.Target{Host: host, Port: 5432, User: user, Password: string(sec.Data["password"])}, nil
}

// finish records a finished run, deletes its cluster unless it failed and
// keepFailedClusters is set, and schedules the next run.
func (r *Reconciler) finish(ctx context.Context, rc *v1alpha1.RestoreCheck, s v1alpha1.RunSummary) (ctrl.Result, error) {
	keep := s.Result == v1alpha1.ResultFailed && rc.Spec.KeepFailedClusters
	if s.ClusterName != "" && !keep {
		cl := cnpg.NewCluster()
		cl.SetNamespace(rc.Namespace)
		cl.SetName(s.ClusterName)
		if err := r.Delete(ctx, cl, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	limit := defaultHistoryLimit
	if rc.Spec.HistoryLimit != nil {
		limit = int(*rc.Spec.HistoryLimit)
	}
	rc.Status.History = append([]v1alpha1.RunSummary{s}, rc.Status.History...)
	if len(rc.Status.History) > limit {
		rc.Status.History = rc.Status.History[:limit]
	}
	rc.Status.LastRun = &s
	if s.Result == v1alpha1.ResultSucceeded {
		t := s.CompletionTime
		rc.Status.LastSuccessTime = &t
	}
	rc.Status.CurrentRun = nil
	rc.Status.ObservedGeneration = rc.Generation

	cond := metav1.Condition{Type: conditionLastRun, ObservedGeneration: rc.Generation}
	if s.Result == v1alpha1.ResultSucceeded {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "Succeeded", "last run succeeded"
		r.Recorder.Eventf(rc, corev1.EventTypeNormal, "RunSucceeded", "Restore check succeeded%s", describe(s))
	} else {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, s.Reason, s.Message
		r.Recorder.Eventf(rc, corev1.EventTypeWarning, "RunFailed", "%s: %s", s.Reason, s.Message)
	}
	meta.SetStatusCondition(&rc.Status.Conditions, cond)

	if _, next, _, err := isDue(rc, r.Now()); err == nil {
		rc.Status.NextRunTime = next
	}
	if err := r.Status().Update(ctx, rc); err != nil {
		return ctrl.Result{}, err
	}
	metrics.RecordRun(rc, s)
	log.FromContext(ctx).Info("restore check finished", "result", s.Result, "reason", s.Reason, "message", s.Message)
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func describe(s v1alpha1.RunSummary) string {
	var parts []string
	if s.RecoveryDuration != nil {
		parts = append(parts, "recovery "+s.RecoveryDuration.Duration.String())
	}
	if s.DataAge != nil {
		parts = append(parts, "data age "+s.DataAge.Duration.String())
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (r *Reconciler) setInvalid(ctx context.Context, rc *v1alpha1.RestoreCheck, err error) error {
	meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
		Type: conditionLastRun, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonInvalidSpec,
		Message: err.Error(), ObservedGeneration: rc.Generation,
	})
	return r.Status().Update(ctx, rc)
}

// deleteClusters removes temporary clusters left by earlier runs (kept failed
// clusters, or leftovers from a controller restart), except keep.
func (r *Reconciler) deleteClusters(ctx context.Context, rc *v1alpha1.RestoreCheck, keep string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(cnpg.ClusterListGVK)
	if err := r.List(ctx, list, client.InNamespace(rc.Namespace), client.MatchingLabels{
		cnpg.LabelManagedBy: cnpg.ManagedByValue, cnpg.LabelRestoreCheck: rc.Name,
	}); err != nil {
		return err
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	for i := range items {
		if items[i].GetName() == keep || !metav1.IsControlledBy(&items[i], rc) {
			continue
		}
		if err := r.Delete(ctx, &items[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, workers int) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.RunChecks == nil {
		r.RunChecks = func(ctx context.Context, t checks.Target, spec v1alpha1.ChecksSpec, start time.Time) checks.Result {
			return checks.Run(ctx, checks.PGXConnector, t, spec, start)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RestoreCheck{}).
		Owns(cnpg.NewCluster()).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}
