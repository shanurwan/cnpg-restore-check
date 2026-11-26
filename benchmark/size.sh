#!/usr/bin/env bash
# Restore time versus database size, and versus WAL to replay.
#   usage: size.sh <pgbench-scale> <rep> [load-seconds]
# Scale 70 is about 1 GB. With load-seconds > 0, pgbench writes for that long after the backup,
# so the restore has that much WAL to replay on top of the base backup.
set -euo pipefail
SCALE=$1; REP=$2; LOAD=${3:-0}
HERE="$(cd "$(dirname "$0")" && pwd)"
SUITE=$([ "$LOAD" -gt 0 ] && echo walreplay || echo size)
NS="${SUITE}-${SCALE}-${LOAD}-${REP}"
OUT="$HERE/results/raw/$SUITE"; mkdir -p "$OUT"
. "$HERE/lib/lib.sh"

STORAGE="$(( SCALE * 32 / 1000 + 3 ))Gi"    # about twice the pgbench data size, plus headroom
T0=$(now)
ns_setup
src_create "$STORAGE"
kubectl -n "$NS" exec src-1 -c postgres -- pgbench -i -s "$SCALE" -q app >/dev/null 2>&1
sql "vacuum (analyze) pgbench_accounts"
sql "checkpoint"
DB_BYTES=$(sql "select pg_database_size('app')")
sql "insert into heartbeat default values" >/dev/null

hb_start
trap 'hb_stop' EXIT
backup_take base
B_START=$(kubectl -n "$NS" get backup base -o jsonpath='{.status.startedAt}')
B_STOP=$(kubectl -n "$NS" get backup base -o jsonpath='{.status.stoppedAt}')
END_LSN=$(kubectl -n "$NS" get backup base -o jsonpath='{.status.endLSN}')

WAL_AFTER=0
if [ "$LOAD" -gt 0 ]; then
  kubectl -n "$NS" exec src-1 -c postgres -- pgbench -c 4 -j 2 -T "$LOAD" app >/dev/null 2>&1
  WAL_AFTER=$(sql "select pg_wal_lsn_diff(pg_current_wal_lsn(), '$END_LSN')::bigint")
fi
wal_flush
S3=$(s3op summary "$RUNID/" src)

T_CHECK=$(now)
check_run "select count(*) = $(( SCALE * 100000 )) from pgbench_accounts" 5m "$STORAGE"
T_DONE=$(now)
hb_stop
trap - EXIT

P="$OUT/.parts-${NS}"; mkdir -p "$P"
echo "$S3" > "$P/bucket.json"
rcheck_view > "$P/restorecheck.json"
leftovers > "$P/leftovers_after_run.json"
python3 "$HERE/lib/merge.py" "$P" "$OUT/${NS}.json" \
  suite="$SUITE" run_id="$RUNID" scale="$SCALE" rep="$REP" load_seconds="$LOAD" storage="$STORAGE" \
  db_bytes="$DB_BYTES" wal_bytes_after_backup="$WAL_AFTER" backup_started="$B_START" backup_stopped="$B_STOP" \
  seconds_setup=$((T_CHECK - T0)) seconds_check=$((T_DONE - T_CHECK))
s3op delete-prefix "$RUNID/" >/dev/null || log "WARN: could not delete bucket prefix $RUNID/"
ns_delete
