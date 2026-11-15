package cnpg

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
)

func rc() *v1alpha1.RestoreCheck {
	return &v1alpha1.RestoreCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "drills"},
		Spec: v1alpha1.RestoreCheckSpec{
			Source: v1alpha1.SourceSpec{ObjectStoreName: "prod-readonly", ServerName: "pg-main"},
		},
	}
}

func TestBuildClusterNeverArchivesOrBacksUp(t *testing.T) {
	r := rc()
	r.Spec.Target = &v1alpha1.TargetSpec{TargetTime: "2026-11-15T00:00:00Z"}
	r.Spec.Cluster.Parameters = map[string]string{"max_connections": "200"}
	r.Spec.Cluster.ServiceAccountAnnotations = map[string]string{"iam.gke.io/gcp-service-account": "restore@p.iam.gserviceaccount.com"}
	u, err := BuildCluster(r, "nightly-x")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"plugins", "backup", "replica"} {
		if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", k); found {
			t.Errorf("spec.%s must not be set on a restore-check cluster", k)
		}
	}
	src, _, _ := unstructured.NestedString(u.Object, "spec", "bootstrap", "recovery", "source")
	ext, _, _ := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	if len(ext) != 1 || ext[0].(map[string]any)["name"] != src {
		t.Fatalf("recovery source %q must match the only external cluster: %v", src, ext)
	}
	params := ext[0].(map[string]any)["plugin"].(map[string]any)["parameters"].(map[string]any)
	if params["barmanObjectName"] != "prod-readonly" || params["serverName"] != "pg-main" {
		t.Errorf("unexpected plugin parameters %v", params)
	}
	tt, _, _ := unstructured.NestedString(u.Object, "spec", "bootstrap", "recovery", "recoveryTarget", "targetTime")
	if tt != "2026-11-15T00:00:00Z" {
		t.Errorf("targetTime = %q", tt)
	}
	mc, _, _ := unstructured.NestedString(u.Object, "spec", "postgresql", "parameters", "max_connections")
	if mc != "200" {
		t.Errorf("max_connections = %q", mc)
	}
	sa, _, _ := unstructured.NestedString(u.Object, "spec", "serviceAccountTemplate", "metadata", "annotations", "iam.gke.io/gcp-service-account")
	if sa != "restore@p.iam.gserviceaccount.com" {
		t.Errorf("service account annotation = %q", sa)
	}
	if u.GetLabels()[LabelRestoreCheck] != "nightly" || u.GetLabels()[LabelManagedBy] != ManagedByValue {
		t.Errorf("labels = %v", u.GetLabels())
	}
}

func TestBuildClusterDefaultsAndResources(t *testing.T) {
	r := rc()
	r.Spec.Cluster.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	u, err := BuildCluster(r, "n")
	if err != nil {
		t.Fatal(err)
	}
	size, _, _ := unstructured.NestedString(u.Object, "spec", "storage", "size")
	if size != "10Gi" {
		t.Errorf("default storage size = %q", size)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "bootstrap", "recovery", "recoveryTarget"); found {
		t.Error("no recoveryTarget expected for the default latest target")
	}
	mem, _, _ := unstructured.NestedString(u.Object, "spec", "resources", "requests", "memory")
	if mem != "1Gi" {
		t.Errorf("memory request = %q", mem)
	}
	if err := unstructured.SetNestedField(u.Object, "x", "spec", "probe"); err != nil {
		t.Fatalf("built object must be valid JSON-compatible data: %v", err)
	}
}

func TestBuildClusterRejectsTwoTargets(t *testing.T) {
	r := rc()
	r.Spec.Target = &v1alpha1.TargetSpec{BackupID: "b1", TargetImmediate: true}
	if _, err := BuildCluster(r, "n"); err == nil {
		t.Fatal("expected an error for two recovery targets")
	}
}

func cluster(name string, labels map[string]string, plugins ...map[string]any) unstructured.Unstructured {
	u := NewCluster()
	u.SetName(name)
	u.SetLabels(labels)
	var ps []any
	for _, p := range plugins {
		ps = append(ps, p)
	}
	if ps != nil {
		u.Object["spec"] = map[string]any{"plugins": ps}
	}
	return *u
}

func TestObjectStoreWriters(t *testing.T) {
	writer := map[string]any{"name": PluginName, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "prod"}}
	disabled := map[string]any{"name": PluginName, "enabled": false, "parameters": map[string]any{"barmanObjectName": "prod"}}
	other := map[string]any{"name": "other.plugin", "parameters": map[string]any{"barmanObjectName": "prod"}}
	clusters := []unstructured.Unstructured{
		cluster("pg-main", nil, writer),
		cluster("pg-off", nil, disabled),
		cluster("pg-other", nil, other),
		cluster("ours", map[string]string{LabelManagedBy: ManagedByValue}, writer),
		cluster("plain", nil),
	}
	got := ObjectStoreWriters(clusters, "prod")
	if len(got) != 1 || got[0] != "pg-main" {
		t.Fatalf("writers = %v, want [pg-main]", got)
	}
	if got := ObjectStoreWriters(clusters, "readonly"); len(got) != 0 {
		t.Fatalf("writers of an unused store = %v", got)
	}
}

func TestIsReady(t *testing.T) {
	u := NewCluster()
	if IsReady(u) {
		t.Fatal("no status is not ready")
	}
	u.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "False"},
	}}
	if IsReady(u) {
		t.Fatal("Ready=False is not ready")
	}
	u.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "True"},
	}}
	if !IsReady(u) {
		t.Fatal("Ready=True is ready")
	}
}
