package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
	"github.com/shanurwan/cnpg-restore-check/internal/checks"
	"github.com/shanurwan/cnpg-restore-check/internal/cnpg"
)

const ns = "drills"

type env struct {
	t      *testing.T
	c      client.Client
	r      *Reconciler
	now    time.Time
	result checks.Result
	calls  int
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []struct{ kind, list string }{{"Cluster", "ClusterList"}} {
		s.AddKnownTypeWithName(cnpg.ClusterGVK.GroupVersion().WithKind(gvk.kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(cnpg.ClusterGVK.GroupVersion().WithKind(gvk.list), &unstructured.UnstructuredList{})
	}
	s.AddKnownTypeWithName(cnpg.ObjectStoreGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(cnpg.ObjectStoreGVK.GroupVersion().WithKind("ObjectStoreList"), &unstructured.UnstructuredList{})

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.RestoreCheck{}).Build()
	e := &env{t: t, c: c, now: time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)}
	e.result = checks.Result{Checks: []v1alpha1.CheckResult{{Name: "connect", Passed: true}}}
	e.r = &Reconciler{
		Client: c, APIReader: c, Scheme: s, Recorder: record.NewFakeRecorder(100),
		Now: func() time.Time { return e.now },
		RunChecks: func(context.Context, checks.Target, v1alpha1.ChecksSpec, time.Time) checks.Result {
			e.calls++
			return e.result
		},
	}
	return e
}

func objectStore(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpg.ObjectStoreGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func restoreCheck(mut ...func(*v1alpha1.RestoreCheck)) *v1alpha1.RestoreCheck {
	rc := &v1alpha1.RestoreCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: ns,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))},
		Spec: v1alpha1.RestoreCheckSpec{
			Source: v1alpha1.SourceSpec{ObjectStoreName: "readonly", ServerName: "pg-main"},
		},
	}
	for _, m := range mut {
		m(rc)
	}
	return rc
}

func (e *env) reconcile() ctrl.Result {
	e.t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "nightly"}})
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *env) get() *v1alpha1.RestoreCheck {
	e.t.Helper()
	rc := &v1alpha1.RestoreCheck{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "nightly"}, rc); err != nil {
		e.t.Fatal(err)
	}
	return rc
}

func (e *env) cluster(name string) (*unstructured.Unstructured, bool) {
	e.t.Helper()
	u := cnpg.NewCluster()
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return u, true
}

func (e *env) markReady(name string) {
	e.t.Helper()
	u, ok := e.cluster(name)
	if !ok {
		e.t.Fatalf("cluster %s missing", name)
	}
	u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if err := e.c.Update(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-superuser", Namespace: ns},
		Data: map[string][]byte{"username": []byte("postgres"), "password": []byte("pw")}}
	if err := e.c.Create(context.Background(), sec); err != nil {
		e.t.Fatal(err)
	}
}

// startRun reconciles once and returns the temporary cluster's name.
func (e *env) startRun() string {
	e.t.Helper()
	e.reconcile()
	run := e.get().Status.CurrentRun
	if run == nil || run.Phase != v1alpha1.PhaseRestoring {
		e.t.Fatalf("expected a run in Restoring, got %+v (last run %+v)", run, e.get().Status.LastRun)
	}
	if _, ok := e.cluster(run.ClusterName); !ok {
		e.t.Fatalf("cluster %s not created", run.ClusterName)
	}
	return run.ClusterName
}

func TestSuccessfulRun(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Objectives = &v1alpha1.ObjectivesSpec{RecoveryTime: &metav1.Duration{Duration: time.Hour}}
	}))
	name := e.startRun()
	if name != "nightly-20261115-030000" {
		t.Errorf("cluster name = %s", name)
	}
	cl, _ := e.cluster(name)
	if len(cl.GetOwnerReferences()) != 1 || cl.GetOwnerReferences()[0].Name != "nightly" {
		t.Errorf("cluster must be owned by the RestoreCheck: %v", cl.GetOwnerReferences())
	}

	// Not ready yet: still restoring, no checks.
	e.now = e.now.Add(time.Minute)
	e.reconcile()
	if e.get().Status.CurrentRun.Phase != v1alpha1.PhaseRestoring || e.calls != 0 {
		t.Fatal("must keep restoring until the cluster is ready")
	}

	e.now = e.now.Add(4 * time.Minute)
	e.markReady(name)
	e.reconcile() // -> Checking
	e.reconcile() // run checks and finish
	st := e.get().Status
	if st.CurrentRun != nil || st.LastRun == nil {
		t.Fatalf("run should be finished: %+v", st)
	}
	if st.LastRun.Result != v1alpha1.ResultSucceeded {
		t.Fatalf("result = %s %s %s", st.LastRun.Result, st.LastRun.Reason, st.LastRun.Message)
	}
	if st.LastRun.RecoveryDuration.Duration != 5*time.Minute {
		t.Errorf("recovery duration = %s, want 5m", st.LastRun.RecoveryDuration.Duration)
	}
	if st.LastSuccessTime == nil || len(st.History) != 1 {
		t.Errorf("lastSuccessTime/history not recorded: %+v", st)
	}
	if _, ok := e.cluster(name); ok {
		t.Error("cluster of a successful run must be deleted")
	}
	if c := meta.FindStatusCondition(st.Conditions, conditionLastRun); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition = %+v", c)
	}

	// No schedule: it does not run again.
	e.now = e.now.Add(24 * time.Hour)
	e.reconcile()
	if e.get().Status.CurrentRun != nil {
		t.Fatal("a check without a schedule runs once")
	}
}

func TestFailedChecksAndKeepFailedCluster(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.KeepFailedClusters = true
	}))
	e.result = checks.Result{Checks: []v1alpha1.CheckResult{
		{Name: "connect", Passed: true},
		{Name: "sql/orders", Passed: false, Message: "assertion returned false"},
	}}
	name := e.startRun()
	e.markReady(name)
	e.reconcile()
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr.Result != v1alpha1.ResultFailed || lr.Reason != v1alpha1.ReasonCheckFailed {
		t.Fatalf("got %s/%s", lr.Result, lr.Reason)
	}
	if !strings.Contains(lr.Message, "sql/orders: assertion returned false") {
		t.Errorf("message = %q", lr.Message)
	}
	if _, ok := e.cluster(name); !ok {
		t.Error("keepFailedClusters must keep the cluster of a failed run")
	}
	if e.get().Status.LastSuccessTime != nil {
		t.Error("a failed run must not set lastSuccessTime")
	}
}

func TestDataAgeObjective(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Checks.Freshness = &v1alpha1.FreshnessCheck{Query: "select max(ts) from heartbeat"}
		rc.Spec.Objectives = &v1alpha1.ObjectivesSpec{DataAge: &metav1.Duration{Duration: 5 * time.Minute}}
	}))
	age := 47 * time.Minute
	e.result = checks.Result{Checks: []v1alpha1.CheckResult{{Name: "freshness", Passed: true}}, DataAge: &age}
	name := e.startRun()
	e.markReady(name)
	e.reconcile()
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr.Result != v1alpha1.ResultFailed || lr.Reason != v1alpha1.ReasonObjectiveMissed {
		t.Fatalf("got %s/%s: %s", lr.Result, lr.Reason, lr.Message)
	}
	if lr.DataAge.Duration != age {
		t.Errorf("data age = %s", lr.DataAge.Duration)
	}
	if !strings.Contains(lr.Message, "47m0s old, objective 5m0s") {
		t.Errorf("message = %q", lr.Message)
	}
}

func TestDataAgeObjectiveNeedsFreshness(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Objectives = &v1alpha1.ObjectivesSpec{DataAge: &metav1.Duration{Duration: time.Minute}}
	}))
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr == nil || lr.Reason != v1alpha1.ReasonInvalidSpec || !strings.Contains(lr.Message, "checks.freshness") {
		t.Fatalf("got %+v", lr)
	}
}

func TestRefusesObjectStoreUsedForArchiving(t *testing.T) {
	src := cnpg.NewCluster()
	src.SetNamespace(ns)
	src.SetName("pg-main")
	src.Object["spec"] = map[string]any{"plugins": []any{map[string]any{
		"name": cnpg.PluginName, "isWALArchiver": true,
		"parameters": map[string]any{"barmanObjectName": "readonly"},
	}}}
	e := newEnv(t, objectStore("readonly"), src, restoreCheck())
	e.reconcile()
	st := e.get().Status
	if st.CurrentRun != nil || st.LastRun == nil || st.LastRun.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("expected InvalidSpec and no run, got %+v", st)
	}
	if !strings.Contains(st.LastRun.Message, "pg-main") {
		t.Errorf("message should name the archiving cluster: %q", st.LastRun.Message)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(cnpg.ClusterListGVK)
	_ = e.c.List(context.Background(), list, client.InNamespace(ns))
	if len(list.Items) != 1 {
		t.Errorf("no temporary cluster may be created, found %d clusters", len(list.Items))
	}
}

func TestMissingObjectStore(t *testing.T) {
	e := newEnv(t, restoreCheck())
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr == nil || lr.Reason != v1alpha1.ReasonInvalidSpec || !strings.Contains(lr.Message, `ObjectStore "readonly" not found`) {
		t.Fatalf("got %+v", lr)
	}
}

func TestRecoveryJobFailure(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck())
	name := e.startRun()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-1-full-recovery", Namespace: ns,
			Labels: map[string]string{"cnpg.io/cluster": name}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}}},
	}
	if err := e.c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr == nil || lr.Reason != v1alpha1.ReasonRecoveryFailed || !strings.Contains(lr.Message, "BackoffLimitExceeded") {
		t.Fatalf("got %+v", lr)
	}
	if _, ok := e.cluster(name); ok {
		t.Error("cluster must be deleted when keepFailedClusters is false")
	}
}

func TestTimeout(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	}))
	e.startRun()
	e.now = e.now.Add(31 * time.Minute)
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr == nil || lr.Reason != v1alpha1.ReasonTimeout {
		t.Fatalf("got %+v", lr)
	}
}

func TestScheduleAndManualTrigger(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Schedule = "0 4 * * *"
	}))
	// A new scheduled check runs once right away.
	name := e.startRun()
	e.markReady(name)
	e.reconcile()
	e.reconcile()
	st := e.get().Status
	if st.LastRun == nil || st.LastRun.Result != v1alpha1.ResultSucceeded {
		t.Fatalf("got %+v", st.LastRun)
	}
	want := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)
	if st.NextRunTime == nil || !st.NextRunTime.Time.Equal(want) {
		t.Fatalf("next run = %v, want %v", st.NextRunTime, want)
	}
	if r := e.reconcile(); r.RequeueAfter != want.Sub(e.now) {
		t.Errorf("requeue after %s, want %s", r.RequeueAfter, want.Sub(e.now))
	}

	// run-now starts a run immediately, once.
	rc := e.get()
	rc.Annotations = map[string]string{v1alpha1.RunNowAnnotation: "1"}
	if err := e.c.Update(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Second)
	e.reconcile()
	st = e.get().Status
	if st.CurrentRun == nil || st.LastManualTrigger != "1" {
		t.Fatalf("run-now did not start a run: %+v", st)
	}
}

func TestInvalidSchedule(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Schedule = "every day"
	}))
	e.reconcile()
	c := meta.FindStatusCondition(e.get().Status.Conditions, conditionLastRun)
	if c == nil || c.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("condition = %+v", c)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	busy := restoreCheck()
	busy.Name = "other"
	busy.Status.CurrentRun = &v1alpha1.CurrentRun{ClusterName: "other-x", Phase: v1alpha1.PhaseRestoring}
	e := newEnv(t, objectStore("readonly"), busy, restoreCheck())
	e.r.MaxConcurrentRuns = 1
	res := e.reconcile()
	if e.get().Status.CurrentRun != nil || res.RequeueAfter != busyRetry {
		t.Fatalf("must wait for the running check, got requeue %s", res.RequeueAfter)
	}
}

func TestClusterNameLength(t *testing.T) {
	n := clusterName(strings.Repeat("a", 60), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if len(n) > 46 || !strings.HasSuffix(n, "-20260102-030405") {
		t.Fatalf("name %q (%d chars)", n, len(n))
	}
}

func (e *env) deleteCluster(name string) {
	e.t.Helper()
	u := cnpg.NewCluster()
	u.SetNamespace(ns)
	u.SetName(name)
	if err := e.c.Delete(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
}

// A cluster that is not visible yet (cache lag, or a crash between the status
// update and the create) is created again, and the run continues.
func TestClusterNotSeenYetIsCreated(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck())
	name := e.startRun()
	e.deleteCluster(name)
	e.reconcile()
	if _, ok := e.cluster(name); !ok {
		t.Fatal("cluster should be created again")
	}
	if st := e.get().Status; st.CurrentRun == nil || st.LastRun != nil {
		t.Fatalf("run should continue: %+v", st)
	}
}

func TestClusterLostAfterSeen(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck())
	name := e.startRun()
	e.reconcile() // observes the cluster and records it
	if e.get().Status.CurrentRun.ClusterUID == "" {
		t.Fatal("cluster UID should be recorded once seen")
	}
	e.deleteCluster(name)
	e.reconcile()
	lr := e.get().Status.LastRun
	if lr == nil || lr.Reason != v1alpha1.ReasonClusterLost {
		t.Fatalf("got %+v", lr)
	}
}

// A reconcile working from a stale copy (no run recorded yet) must not delete
// or create clusters: its status update conflicts first.
func TestStaleStartTouchesNothing(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck())
	stale := e.get()
	name := e.startRun()
	if _, err := e.r.start(context.Background(), stale, e.now.Add(time.Second), ""); err == nil {
		t.Fatal("start from a stale copy must fail with a conflict")
	} else if !apierrors.IsConflict(err) {
		t.Fatalf("want a conflict, got %v", err)
	}
	if _, ok := e.cluster(name); !ok {
		t.Fatal("the running cluster must not be deleted by a stale reconcile")
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(cnpg.ClusterListGVK)
	_ = e.c.List(context.Background(), list, client.InNamespace(ns))
	if len(list.Items) != 1 {
		t.Fatalf("a stale reconcile must not create clusters, found %d", len(list.Items))
	}
}

// A reconcile that finishes a run from a stale copy, after another reconcile
// already recorded it and deleted the cluster, must not emit an event. Seen on
// a lab: "RunFailed ClusterLost" right after "RunSucceeded" for the same run.
func TestStaleFinishEmitsNoEvent(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck())
	name := e.startRun()
	e.markReady(name)
	e.reconcile() // -> Checking, cluster UID recorded
	stale := e.get()
	e.reconcile() // runs the checks, records success, deletes the cluster

	rec := e.r.Recorder.(*record.FakeRecorder)
	for len(rec.Events) > 0 {
		<-rec.Events
	}

	res, err := e.r.progress(context.Background(), stale)
	if err != nil {
		t.Fatalf("a stale reconcile should requeue quietly, got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("a stale reconcile should look again")
	}
	if len(rec.Events) != 0 {
		t.Fatalf("a stale reconcile emitted %q", <-rec.Events)
	}
	if lr := e.get().Status.LastRun; lr == nil || lr.Result != v1alpha1.ResultSucceeded {
		t.Fatalf("the recorded run must stay a success, got %+v", lr)
	}
}

func TestNewScheduledCheckRunsImmediatelyThenFollowsSchedule(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Schedule = "0 * * * *"
		rc.CreationTimestamp = metav1.NewTime(time.Date(2026, 11, 15, 2, 59, 0, 0, time.UTC))
	}))
	e.now = time.Date(2026, 11, 15, 3, 10, 0, 0, time.UTC)
	name := e.startRun()
	e.markReady(name)
	e.reconcile()
	e.reconcile()
	e.now = e.now.Add(5 * time.Minute)
	e.reconcile()
	st := e.get().Status
	if st.CurrentRun != nil {
		t.Fatal("after the first run, the next one waits for the schedule")
	}
	if want := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC); st.NextRunTime == nil || !st.NextRunTime.Time.Equal(want) {
		t.Fatalf("next run = %v, want %v", st.NextRunTime, want)
	}
}

func TestSuspendedNewCheckDoesNotRun(t *testing.T) {
	e := newEnv(t, objectStore("readonly"), restoreCheck(func(rc *v1alpha1.RestoreCheck) {
		rc.Spec.Schedule = "0 * * * *"
		rc.Spec.Suspend = true
	}))
	e.reconcile()
	if e.get().Status.CurrentRun != nil {
		t.Fatal("a suspended check must not start")
	}
}
