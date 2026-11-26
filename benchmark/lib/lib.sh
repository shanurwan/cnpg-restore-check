# Shared helpers for the benchmark scripts. Source this file; it expects NS and OUT to be set.
set -euo pipefail
BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { echo "$(date -u +%H:%M:%S) [${NS}] $*" >&2; }
now() { date -u +%s; }

s3op() { kubectl -n bench-s3 exec s3tool -- python /tmp/s3ops.py "$@"; }

# Namespace with S3 credentials and two ObjectStores pointing at the same prefix:
# "archive" is used by the source cluster, "restore-source" by the RestoreCheck.
ns_setup() {
  # A namespace with the same name may still be terminating from an earlier run.
  for i in $(seq 1 60); do kubectl get namespace "$NS" >/dev/null 2>&1 || break; sleep 5; done
  kubectl create namespace "$NS" >/dev/null
  kubectl -n bench-s3 get secret s3 -o json \
    | python3 -c 'import json,sys; s=json.load(sys.stdin); print(json.dumps({"apiVersion":"v1","kind":"Secret","metadata":{"name":"s3"},"type":"Opaque","data":s["data"]}))' \
    | kubectl -n "$NS" apply -f - >/dev/null
  # A fresh bucket prefix for every run: barman-cloud refuses to archive into a prefix that already has WAL.
  RUNID="${NS}-$(date -u +%Y%m%d%H%M%S)"
  sed -e "s|PREFIX|${RUNID}|g" "$BENCH_DIR/manifests/objectstores.yaml" | kubectl -n "$NS" apply -f - >/dev/null
}

# Source cluster "src" with WAL archiving, a heartbeat table and an orders table.
src_create() {          # storage size, e.g. 2Gi
  sed -e "s|STORAGE|$1|g" "$BENCH_DIR/manifests/source-cluster.yaml" | kubectl -n "$NS" apply -f - >/dev/null
  kubectl -n "$NS" wait cluster/src --for=condition=Ready --timeout=15m >/dev/null
}

sql() { kubectl -n "$NS" exec src-1 -c postgres -- psql -d app -tAqX -v ON_ERROR_STOP=1 -c "$1"; }

# Heartbeat writer: a row every 2 s and a WAL switch every 10 s, so archived WAL is never far behind.
hb_start() {
  ( while true; do
      for i in 1 2 3 4; do sql "insert into heartbeat default values" >/dev/null 2>&1 || true; sleep 2; done
      sql "insert into heartbeat default values; select pg_switch_wal()" >/dev/null 2>&1 || true; sleep 2
    done ) &
  HB_PID=$!
}
hb_stop() { kill "$HB_PID" 2>/dev/null || true; wait "$HB_PID" 2>/dev/null || true; }

backup_take() {         # name
  sed -e "s|NAME|$1|g" "$BENCH_DIR/manifests/backup.yaml" | kubectl -n "$NS" apply -f - >/dev/null
  kubectl -n "$NS" wait "backup/$1" --for=jsonpath='{.status.phase}'=completed --timeout=30m >/dev/null
}

# Wait until everything written up to now is in the archive: switch WAL and wait for the archiver to report it.
wal_flush() {
  local seg
  seg=$(sql "select pg_walfile_name(pg_current_wal_lsn())")
  sql "select pg_switch_wal()" >/dev/null
  for i in $(seq 1 60); do
    [ "$(sql "select coalesce(last_archived_wal,'') >= '$seg' from pg_stat_archiver")" = t ] && return 0
    sleep 2
  done
  log "WARN: archiver did not reach $seg"
}

# Create the RestoreCheck (its first run starts right away) and wait for that run to finish.
check_run() {           # sql-check-query data-age-objective storage-size
  sed -e "s|SQLCHECK|$1|g" -e "s|DATAAGE|$2|g" -e "s|STORAGE|$3|g" "$BENCH_DIR/manifests/restorecheck.yaml" \
    | kubectl -n "$NS" apply -f - >/dev/null
  for i in $(seq 1 1440); do
    [ -n "$(kubectl -n "$NS" get restorecheck src -o jsonpath='{.status.lastRun.completionTime}' 2>/dev/null)" ] && return 0
    sleep 5
  done
  log "WARN: run did not finish in 2 h"
}

# What CloudNativePG itself says, recorded at verdict time.
cnpg_view() {
  kubectl -n "$NS" get backups.postgresql.cnpg.io -o json | python3 -c '
import json,sys
b=json.load(sys.stdin)["items"]
print(json.dumps([{"name":x["metadata"]["name"],"phase":x.get("status",{}).get("phase"),"error":x.get("status",{}).get("error")} for x in b]))'
}
cnpg_conditions() {
  kubectl -n "$NS" get cluster src -o json | python3 -c '
import json,sys
c=json.load(sys.stdin)
print(json.dumps({"phase":c.get("status",{}).get("phase"),
 "conditions":{x["type"]:{"status":x["status"],"reason":x.get("reason"),"message":(x.get("message") or "")[:200]} for x in c.get("status",{}).get("conditions",[])}}))'
}
rcheck_view() { kubectl -n "$NS" get restorecheck src -o json | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin).get("status",{})))'; }

# Anything the run should have cleaned up, besides the source cluster.
leftovers() {
  python3 - "$NS" <<'PY'
import json, subprocess, sys
ns = sys.argv[1]
def names(kind):
    out = subprocess.run(["kubectl", "-n", ns, "get", kind, "-o", "json"], capture_output=True, text=True).stdout
    return [i["metadata"]["name"] for i in json.loads(out or '{"items":[]}')["items"]]
left = {
  "clusters": [n for n in names("clusters.postgresql.cnpg.io") if n != "src"],
  "pvcs": [n for n in names("pvc") if not n.startswith("src-")],
  "pods": [n for n in names("pods") if not n.startswith("src-")],
  "jobs": names("jobs"),
}
print(json.dumps(left))
PY
}

ns_delete() { kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true; }
