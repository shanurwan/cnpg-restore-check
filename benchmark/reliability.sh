#!/usr/bin/env bash
# N back-to-back runs of one RestoreCheck against a healthy database: success rate, timing spread,
# anything left behind after each run, and the operator's own memory and CPU.
#   usage: reliability.sh <runs>
set -euo pipefail
RUNS=${1:-30}
HERE="$(cd "$(dirname "$0")" && pwd)"
NS="rel-$(date -u +%H%M)"
OUT="$HERE/results/raw/reliability"; mkdir -p "$OUT"
. "$HERE/lib/lib.sh"

ns_setup
src_create 2Gi
sql "insert into orders(note) select md5(i::text) from generate_series(1, 100000) i; insert into heartbeat default values"
hb_start
trap 'hb_stop' EXIT
backup_take base
wal_flush
check_run "select count(*) = 100000 from orders" 60s 2Gi     # run 1 (starts on create)

record() {   # run number
  local P="$OUT/.parts-$1"; mkdir -p "$P"
  kubectl -n "$NS" get restorecheck src -o json | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["status"]["lastRun"]))' > "$P/last_run.json"
  leftovers > "$P/leftovers_after_run.json"
  kubectl -n cnpg-restore-check top pod --no-headers 2>/dev/null | awk '{print "{\"cpu\":\""$2"\",\"memory\":\""$3"\"}"}' | head -1 > "$P/operator.json"
  python3 "$HERE/lib/merge.py" "$P" "$OUT/run-$(printf %02d "$1").json" suite=reliability run_id="$RUNID" run_number="$1"
}
record 1
for n in $(seq 2 "$RUNS"); do
  before=$(kubectl -n "$NS" get restorecheck src -o jsonpath='{.status.lastRun.completionTime}')
  kubectl -n "$NS" annotate restorecheck src --overwrite restorecheck.shanurwan.github.io/run-now="$(date +%s)" >/dev/null
  for i in $(seq 1 720); do
    cur=$(kubectl -n "$NS" get restorecheck src -o jsonpath='{.status.lastRun.completionTime}')
    [ "$cur" != "$before" ] && [ -z "$(kubectl -n "$NS" get restorecheck src -o jsonpath='{.status.currentRun.clusterName}')" ] && break
    sleep 5
  done
  record "$n"
  log "run $n done"
done
hb_stop
trap - EXIT
s3op delete-prefix "$RUNID/" >/dev/null || log "WARN: could not delete bucket prefix $RUNID/"
ns_delete
