//go:build e2e

// Package e2e runs restore checks against a real cluster. It needs the current
// kubectl context to have CloudNativePG, plugin-barman-cloud and the
// cnpg-restore-check controller installed. See README.md in this directory.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	ns         = fmt.Sprintf("crc-e2e-%d", time.Now().Unix()%100000)
	ctrlNS     = envOr("CRC_NAMESPACE", "cnpg-restore-check")
	ctrlSvc    = envOr("CRC_METRICS_SERVICE", "cnpg-restore-check-metrics")
	keepOnFail = os.Getenv("CRC_E2E_KEEP") != ""
)

const (
	s3Image  = "docker.io/rustfs/rustfs@sha256:f9697e5401145bc3bc3926ea0ab3b39b0dabf8a4e8c28b125dde62cf264350e7" // 1.0.5-glibc
	awsImage = "docker.io/amazon/aws-cli:2.31.13"
	orders   = 10000
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func kubectl(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func must(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := kubectl(t, stdin, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func apply(t *testing.T, yaml string) {
	t.Helper()
	must(t, yaml, "apply", "-n", ns, "-f", "-")
}

func eventually(t *testing.T, timeout time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, state := f()
		if ok {
			return
		}
		last = state
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s (last state: %s)", timeout, what, last)
}

func psql(t *testing.T, sql string) string {
	t.Helper()
	out := must(t, "", "exec", "-n", ns, "src-1", "-c", "postgres", "--",
		"psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", "app", "-tAc", sql)
	return strings.TrimSpace(out)
}

// awsS3 runs an aws s3api command against the in-namespace S3 server.
func awsS3(t *testing.T, args ...string) string {
	t.Helper()
	name := fmt.Sprintf("aws-%d", time.Now().UnixNano()%1000000)
	overrides := fmt.Sprintf(`{"spec":{"containers":[{"name":"aws","image":%q,"args":%s,
	  "env":[{"name":"AWS_ACCESS_KEY_ID","value":"e2e-access"},{"name":"AWS_SECRET_ACCESS_KEY","value":"e2e-secret-key"},
	         {"name":"AWS_DEFAULT_REGION","value":"us-east-1"}]}]}}`,
		awsImage, mustJSON(append([]string{"--endpoint-url", "http://s3:9000", "s3api"}, args...)))
	return must(t, "", "run", name, "-n", ns, "--rm", "-i", "--restart=Never", "--quiet",
		"--image", awsImage, "--overrides", overrides)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type runSummary struct {
	ClusterName      string `json:"clusterName"`
	Result           string `json:"result"`
	Reason           string `json:"reason"`
	Message          string `json:"message"`
	RecoveryDuration string `json:"recoveryDuration"`
	DataAge          string `json:"dataAge"`
	Checks           []struct {
		Name    string `json:"name"`
		Passed  bool   `json:"passed"`
		Message string `json:"message"`
	} `json:"checks"`
}

func waitLastRun(t *testing.T, name string, timeout time.Duration) runSummary {
	t.Helper()
	var s runSummary
	eventually(t, timeout, "RestoreCheck "+name+" to finish a run", func() (bool, string) {
		out, err := kubectl(t, "", "get", "restorecheck", "-n", ns, name, "-o", "jsonpath={.status.lastRun}")
		if err != nil || out == "" {
			phase, _ := kubectl(t, "", "get", "restorecheck", "-n", ns, name, "-o", "jsonpath={.status.currentRun.phase}")
			return false, "phase=" + phase
		}
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatal(err)
		}
		return true, ""
	})
	t.Logf("%s: result=%s reason=%s recovery=%s dataAge=%s message=%q", name, s.Result, s.Reason, s.RecoveryDuration, s.DataAge, s.Message)
	return s
}

func managedClusters(t *testing.T, rc string) []string {
	out := must(t, "", "get", "clusters.postgresql.cnpg.io", "-n", ns,
		"-l", "restorecheck.shanurwan.github.io/name="+rc, "-o", "jsonpath={.items[*].metadata.name}")
	return strings.Fields(out)
}

func TestRestoreChecks(t *testing.T) {
	if _, err := kubectl(t, "", "get", "deploy", "-n", ctrlNS); err != nil {
		t.Fatalf("controller namespace %q not reachable: %v", ctrlNS, err)
	}
	must(t, "", "create", "namespace", ns)
	t.Cleanup(func() {
		if t.Failed() && keepOnFail {
			t.Logf("keeping namespace %s for debugging", ns)
			return
		}
		_, _ = kubectl(t, "", "delete", "namespace", ns, "--wait=false")
	})

	setupS3(t)
	setupSource(t)

	t.Run("refuses the object store the source archives to", func(t *testing.T) {
		apply(t, restoreCheck("guard", "rw-store", "src", ""))
		s := waitLastRun(t, "guard", 2*time.Minute)
		if s.Result != "Failed" || s.Reason != "InvalidSpec" || !strings.Contains(s.Message, "src") {
			t.Fatalf("want Failed/InvalidSpec naming cluster src, got %+v", s)
		}
		if c := managedClusters(t, "guard"); len(c) != 0 {
			t.Fatalf("no cluster may be created, found %v", c)
		}
	})

	t.Run("healthy backup passes and reports recovery time and data age", func(t *testing.T) {
		apply(t, restoreCheck("healthy", "ro-store", "src", `
  checks:
    amcheck:
      heap: true
    sql:
      - name: orders-restored
        database: app
        query: "select count(*) = `+fmt.Sprint(orders)+` from orders"
    freshness:
      database: app
      query: "select max(ts) from heartbeat"
  objectives:
    recoveryTime: 15m
    dataAge: 10m
    successAge: 26h`))
		s := waitLastRun(t, "healthy", 20*time.Minute)
		if s.Result != "Succeeded" {
			t.Fatalf("want Succeeded, got %+v", s)
		}
		if s.RecoveryDuration == "" || s.DataAge == "" {
			t.Fatalf("recovery duration and data age must be measured: %+v", s)
		}
		names := map[string]bool{}
		for _, c := range s.Checks {
			names[c.Name] = c.Passed
		}
		for _, want := range []string{"connect", "amcheck/app", "amcheck/postgres", "sql/orders-restored", "freshness"} {
			if !names[want] {
				t.Errorf("check %s missing or failed: %+v", want, s.Checks)
			}
		}
		eventually(t, 3*time.Minute, "temporary cluster deletion", func() (bool, string) {
			c := managedClusters(t, "healthy")
			return len(c) == 0, strings.Join(c, ",")
		})
		metrics := must(t, "", "get", "--raw",
			fmt.Sprintf("/api/v1/namespaces/%s/services/%s:metrics/proxy/metrics", ctrlNS, ctrlSvc))
		want := fmt.Sprintf(`cnpg_restore_check_last_run_success{name="healthy",namespace="%s"} 1`, ns)
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q", want)
		}
	})

	t.Run("missing WAL segment: restore comes up but data age objective fails", func(t *testing.T) {
		// Three WAL segments: the first holds a row dated an hour ago, the
		// second and third hold current rows. Deleting the second from the
		// archive makes recovery stop after the first, without an error.
		psql(t, "create table wal_probe(ts timestamptz not null)")
		seg1 := insertAndSwitch(t, "now() - interval '1 hour'")
		seg2 := insertAndSwitch(t, "now()")
		seg3 := insertAndSwitch(t, "now()")
		if seg1 == seg2 || seg2 == seg3 {
			t.Fatalf("rows must be in separate segments: %s %s %s", seg1, seg2, seg3)
		}
		waitArchived(t, seg3)
		deleteWAL(t, "src", seg2)

		apply(t, restoreCheck("walgap", "ro-store", "src", `
  checks:
    freshness:
      database: app
      query: "select max(ts) from wal_probe"
  objectives:
    dataAge: 10m`))
		s := waitLastRun(t, "walgap", 20*time.Minute)
		if s.Result != "Failed" || s.Reason != "ObjectiveMissed" {
			t.Fatalf("want Failed/ObjectiveMissed, got %+v", s)
		}
		if s.RecoveryDuration == "" {
			t.Fatalf("the restore itself must have come up (that is the point): %+v", s)
		}
		age, err := time.ParseDuration(s.DataAge)
		if err != nil || age < 50*time.Minute {
			t.Fatalf("data age should be about an hour, got %q", s.DataAge)
		}
	})

	t.Run("unknown server name fails recovery", func(t *testing.T) {
		apply(t, restoreCheck("noserver", "ro-store", "no-such-server", `
  timeout: 15m`))
		s := waitLastRun(t, "noserver", 20*time.Minute)
		if s.Result != "Failed" || s.Reason != "RecoveryFailed" {
			t.Fatalf("want Failed/RecoveryFailed, got %+v", s)
		}
	})
}

func restoreCheck(name, store, server, extra string) string {
	return fmt.Sprintf(`apiVersion: restorecheck.shanurwan.github.io/v1alpha1
kind: RestoreCheck
metadata:
  name: %s
spec:
  source:
    objectStoreName: %s
    serverName: %s
  cluster:
    storageSize: 1Gi
%s
`, name, store, server, extra)
}

func insertAndSwitch(t *testing.T, ts string) string {
	t.Helper()
	seg := psql(t, fmt.Sprintf("insert into wal_probe values (%s); select pg_walfile_name(pg_current_wal_insert_lsn())", ts))
	psql(t, "select pg_switch_wal()")
	return seg
}

func waitArchived(t *testing.T, seg string) {
	t.Helper()
	eventually(t, 5*time.Minute, "WAL "+seg+" archived", func() (bool, string) {
		last := psql(t, "select coalesce(last_archived_wal, '') from pg_stat_archiver")
		return last >= seg && len(last) == len(seg), last
	})
}

func deleteWAL(t *testing.T, server, seg string) {
	t.Helper()
	prefix := fmt.Sprintf("%s/wals/%s/%s", server, seg[:16], seg)
	keys := strings.Fields(awsS3(t, "list-objects-v2", "--bucket", "backups", "--prefix", prefix,
		"--query", "Contents[].Key", "--output", "text"))
	if len(keys) == 0 || keys[0] == "None" {
		t.Fatalf("no archived object for %s", prefix)
	}
	for _, k := range keys {
		awsS3(t, "delete-object", "--bucket", "backups", "--key", k)
		t.Logf("deleted s3://backups/%s", k)
	}
}

func setupS3(t *testing.T) {
	apply(t, fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: s3
stringData:
  ACCESS_KEY_ID: e2e-access
  ACCESS_SECRET_KEY: e2e-secret-key
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 2Gi
---
apiVersion: v1
kind: Service
metadata:
  name: s3
spec:
  selector:
    app: s3
  ports:
    - port: 9000
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: s3
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: s3
  template:
    metadata:
      labels:
        app: s3
    spec:
      initContainers:
        - name: init
          image: docker.io/library/busybox:1.37
          command: ["sh", "-c", "mkdir -p /data/rustfs /logs/rustfs && chmod 0777 /data/rustfs /logs/rustfs"]
          volumeMounts:
            - {name: data, mountPath: /data}
            - {name: logs, mountPath: /logs}
      containers:
        - name: s3
          image: %s
          command: ["/usr/bin/rustfs"]
          env:
            - {name: RUSTFS_ADDRESS, value: ":9000"}
            - {name: RUSTFS_VOLUMES, value: /data/rustfs}
            - {name: RUSTFS_REGION, value: us-east-1}
            - {name: RUSTFS_CONSOLE_ENABLE, value: "false"}
            - {name: RUSTFS_OBS_LOG_DIRECTORY, value: /logs/rustfs}
            - name: RUSTFS_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: s3, key: ACCESS_KEY_ID}}
            - name: RUSTFS_SECRET_KEY
              valueFrom: {secretKeyRef: {name: s3, key: ACCESS_SECRET_KEY}}
          ports:
            - containerPort: 9000
          readinessProbe:
            httpGet: {path: /health, port: 9000}
            periodSeconds: 5
          volumeMounts:
            - {name: data, mountPath: /data}
            - {name: logs, mountPath: /logs}
      volumes:
        - name: data
          persistentVolumeClaim: {claimName: s3}
        - name: logs
          emptyDir: {}
`, s3Image))
	must(t, "", "rollout", "status", "-n", ns, "deploy/s3", "--timeout=5m")

	// Two ObjectStores on the same bucket: rw-store for the source cluster's
	// archiving, ro-store for restore checks. In production ro-store would use
	// credentials that can only read.
	for _, n := range []string{"rw-store", "ro-store"} {
		apply(t, fmt.Sprintf(`apiVersion: barmancloud.cnpg.io/v1
kind: ObjectStore
metadata:
  name: %s
spec:
  configuration:
    destinationPath: s3://backups/
    endpointURL: http://s3:9000
    s3Credentials:
      accessKeyId: {name: s3, key: ACCESS_KEY_ID}
      secretAccessKey: {name: s3, key: ACCESS_SECRET_KEY}
`, n))
	}
	awsS3(t, "create-bucket", "--bucket", "backups")
}

func setupSource(t *testing.T) {
	apply(t, `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: src
spec:
  instances: 1
  storage:
    size: 1Gi
  plugins:
    - name: barman-cloud.cloudnative-pg.io
      isWALArchiver: true
      parameters:
        barmanObjectName: rw-store
`)
	eventually(t, 10*time.Minute, "source cluster ready", func() (bool, string) {
		out, _ := kubectl(t, "", "get", "clusters.postgresql.cnpg.io", "-n", ns, "src",
			"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		return out == "True", out
	})
	psql(t, fmt.Sprintf(`create table orders(id bigserial primary key, note text not null);
insert into orders(note) select md5(i::text) from generate_series(1, %d) i;
create index orders_note on orders(note);
create table heartbeat(ts timestamptz not null);
insert into heartbeat values (now());`, orders))

	apply(t, `apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata:
  name: src-base
spec:
  cluster:
    name: src
  method: plugin
  pluginConfiguration:
    name: barman-cloud.cloudnative-pg.io
`)
	eventually(t, 10*time.Minute, "base backup completed", func() (bool, string) {
		out, _ := kubectl(t, "", "get", "backups.postgresql.cnpg.io", "-n", ns, "src-base", "-o", "jsonpath={.status.phase}")
		if out == "failed" {
			t.Fatalf("base backup failed")
		}
		return out == "completed", out
	})
	seg := psql(t, "insert into heartbeat values (now()); select pg_walfile_name(pg_current_wal_insert_lsn())")
	psql(t, "select pg_switch_wal()")
	waitArchived(t, seg)
}
