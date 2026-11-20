// Package cnpg builds and inspects CloudNativePG objects as unstructured data,
// so the controller does not pin a CloudNativePG API version.
package cnpg

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/shanurwan/cnpg-restore-check/api/v1alpha1"
)

// PluginName is the plugin-barman-cloud CNPG-I plugin name.
const PluginName = "barman-cloud.cloudnative-pg.io"

// Labels set on every temporary cluster.
const (
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelRestoreCheck = "restorecheck.shanurwan.github.io/name"
	ManagedByValue    = "cnpg-restore-check"
	originName        = "origin"
)

var (
	ClusterGVK     = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	ClusterListGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ClusterList"}
	ObjectStoreGVK = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}
)

// NewCluster returns an empty unstructured Cluster for Get calls.
func NewCluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ClusterGVK)
	return u
}

// BuildCluster builds the temporary cluster for a run. It never sets
// spec.plugins or spec.backup, so the restored cluster cannot archive WAL or
// take backups into any object store.
func BuildCluster(rc *v1alpha1.RestoreCheck, name string) (*unstructured.Unstructured, error) {
	t := rc.Spec.Cluster
	size := t.StorageSize
	if size == "" {
		size = "10Gi"
	}
	storage := map[string]any{"size": size}
	if t.StorageClass != nil {
		storage["storageClass"] = *t.StorageClass
	}

	recovery := map[string]any{"source": originName}
	if tg := rc.Spec.Target; tg != nil {
		set := 0
		target := map[string]any{}
		if tg.BackupID != "" {
			set++
			target["backupID"] = tg.BackupID
		}
		if tg.TargetTime != "" {
			set++
			target["targetTime"] = tg.TargetTime
		}
		if tg.TargetImmediate {
			set++
			target["targetImmediate"] = true
		}
		if set > 1 {
			return nil, fmt.Errorf("spec.target: set at most one of backupID, targetTime, targetImmediate")
		}
		if set == 1 {
			recovery["recoveryTarget"] = target
		}
	}

	spec := map[string]any{
		"instances":             int64(1),
		"enableSuperuserAccess": true,
		"storage":               storage,
		"bootstrap":             map[string]any{"recovery": recovery},
		"externalClusters": []any{
			map[string]any{
				"name": originName,
				"plugin": map[string]any{
					"name": PluginName,
					"parameters": map[string]any{
						"barmanObjectName": rc.Spec.Source.ObjectStoreName,
						"serverName":       rc.Spec.Source.ServerName,
					},
				},
			},
		},
	}
	if t.ImageName != "" {
		spec["imageName"] = t.ImageName
	}
	if len(t.Parameters) > 0 {
		params := map[string]any{}
		for k, v := range t.Parameters {
			params[k] = v
		}
		spec["postgresql"] = map[string]any{"parameters": params}
	}
	if len(t.ServiceAccountAnnotations) > 0 {
		ann := map[string]any{}
		for k, v := range t.ServiceAccountAnnotations {
			ann[k] = v
		}
		spec["serviceAccountTemplate"] = map[string]any{"metadata": map[string]any{"annotations": ann}}
	}
	if len(t.NodeSelector) > 0 {
		ns := map[string]any{}
		for k, v := range t.NodeSelector {
			ns[k] = v
		}
		spec["affinity"] = map[string]any{"nodeSelector": ns}
	}
	res := map[string]any{}
	if len(t.Resources.Requests) > 0 {
		req := map[string]any{}
		for k, v := range t.Resources.Requests {
			req[string(k)] = v.String()
		}
		res["requests"] = req
	}
	if len(t.Resources.Limits) > 0 {
		lim := map[string]any{}
		for k, v := range t.Resources.Limits {
			lim[string(k)] = v.String()
		}
		res["limits"] = lim
	}
	if len(res) > 0 {
		spec["resources"] = res
	}

	u := NewCluster()
	u.SetName(name)
	u.SetNamespace(rc.Namespace)
	u.SetLabels(map[string]string{
		LabelManagedBy:    ManagedByValue,
		LabelRestoreCheck: rc.Name,
	})
	u.Object["spec"] = spec
	return u, nil
}

// IsReady reports whether the cluster's Ready condition is True.
func IsReady(u *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// Phase returns status.phase and status.phaseReason.
func Phase(u *unstructured.Unstructured) (string, string) {
	p, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	r, _, _ := unstructured.NestedString(u.Object, "status", "phaseReason")
	return p, r
}

// ObjectStoreWriters returns the names of clusters (other than those managed
// by this controller) that reference objectStore in spec.plugins, that is,
// clusters that archive WAL or write backups to it.
func ObjectStoreWriters(clusters []unstructured.Unstructured, objectStore string) []string {
	var users []string
	for _, c := range clusters {
		if c.GetLabels()[LabelManagedBy] == ManagedByValue {
			continue
		}
		plugins, _, _ := unstructured.NestedSlice(c.Object, "spec", "plugins")
		for _, p := range plugins {
			m, ok := p.(map[string]any)
			if !ok || m["name"] != PluginName {
				continue
			}
			if enabled, ok := m["enabled"].(bool); ok && !enabled {
				continue
			}
			params, _ := m["parameters"].(map[string]any)
			if params["barmanObjectName"] == objectStore {
				users = append(users, c.GetName())
				break
			}
		}
	}
	return users
}

// RecoveringFrom returns the names of clusters (other than those managed by
// this controller) that are still bootstrapping by recovery from objectStore
// through plugin-barman-cloud. A cluster counts as recovering until it has a
// running primary (status.currentPrimary): CloudNativePG sets that only after
// the recovery job has restored the base backup and replayed WAL.
func RecoveringFrom(clusters []unstructured.Unstructured, objectStore string) []string {
	var names []string
	for _, c := range clusters {
		if c.GetLabels()[LabelManagedBy] == ManagedByValue {
			continue
		}
		if p, _, _ := unstructured.NestedString(c.Object, "status", "currentPrimary"); p != "" {
			continue
		}
		source, _, _ := unstructured.NestedString(c.Object, "spec", "bootstrap", "recovery", "source")
		if source == "" {
			continue
		}
		externals, _, _ := unstructured.NestedSlice(c.Object, "spec", "externalClusters")
		for _, e := range externals {
			m, ok := e.(map[string]any)
			if !ok || m["name"] != source {
				continue
			}
			plugin, _ := m["plugin"].(map[string]any)
			params, _ := plugin["parameters"].(map[string]any)
			if plugin["name"] == PluginName && params["barmanObjectName"] == objectStore {
				names = append(names, c.GetName())
			}
			break
		}
	}
	return names
}
