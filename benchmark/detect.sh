#!/usr/bin/env bash
# One detection run: build a source database, break its backup in one way, run restore-check,
# and record what CloudNativePG reports next to what restore-check reports.
#   usage: detect.sh <scenario> <rep>
#   CHECKER=cronjob runs the do-it-yourself baseline (manifests/baseline-cronjob.yaml) instead of restore-check.
set -euo pipefail
SCEN=$1; REP=$2
CHECKER=${CHECKER:-restorecheck}
HERE="$(cd "$(dirname "$0")" && pwd)"
if [ "$CHECKER" = cronjob ]; then NS="cj-${SCEN}-${REP}"; OUT="$HERE/results/raw/baseline"
else NS="det-${SCEN}-${REP}"; OUT="$HERE/results/raw/detection"; fi
mkdir -p "$OUT"
. "$HERE/lib/lib.sh"

ROWS=100000
T0=$(now)
ns_setup
src_create 2Gi
sql "insert into orders(note) select md5(i::text) from generate_series(1, $ROWS) i; insert into heartbeat default values"
# Freeze and checkpoint "orders" so none of its pages is rewritten after the backup starts:
# otherwise a full-page image in WAL would repair a damaged page during recovery.
sql "vacuum (freeze, analyze) orders"
sql "checkpoint"
CHECKSUMS=$(sql "show data_checksums")
RELPATH=$(sql "select pg_relation_filepath('orders')")
PGVERSION=$(sql "show server_version")
FAULT='{"applied":"none"}'

if [ "$SCEN" = logical-loss ]; then
  # An application bug deletes half the rows before the backup: the backup is valid, the data is not.
  N=$(sql "with d as (delete from orders where id % 2 = 0 returning 1) select count(*) from d")
  FAULT="{\"applied\":\"deleted rows before backup\",\"rows_deleted\":$N}"
fi

hb_start
trap 'hb_stop' EXIT
backup_take base
log "backup completed"
sleep 20

case $SCEN in
  healthy|logical-loss) ;;
  wal-gap)
    # A WAL segment written after the backup goes missing from the bucket.
    sleep 30
    SEG=$(sql "select pg_walfile_name(pg_current_wal_lsn())")
    wal_flush
    FAULT=$(s3op delete-wal "$RUNID/" src "$SEG")
    sleep 90 ;;
  backup-wal-missing)
    # The first WAL segment the base backup needs to become consistent goes missing.
    wal_flush
    FAULT=$(s3op delete-backup-begin-wal "$RUNID/" src) ;;
  base-data-deleted)
    # The data files of the base backup go missing; its backup.info stays.
    FAULT=$(s3op delete-base-data "$RUNID/" src) ;;
  heap-corruption)
    # Silent damage inside the base backup: 16 line pointers of one heap page of "orders" are overwritten.
    FAULT=$(s3op corrupt-heap "$RUNID/" src "$RELPATH" 3) ;;
  archiving-stopped)
    # WAL archiving starts failing (wrong credentials); writes continue for two minutes.
    kubectl -n "$NS" create secret generic s3-wrong --from-literal=ACCESS_KEY_ID=wrong --from-literal=ACCESS_SECRET_KEY=wrong >/dev/null
    kubectl -n "$NS" patch objectstore archive --type merge -p \
      '{"spec":{"configuration":{"s3Credentials":{"accessKeyId":{"name":"s3-wrong","key":"ACCESS_KEY_ID"},"secretAccessKey":{"name":"s3-wrong","key":"ACCESS_SECRET_KEY"}}}}}' >/dev/null
    BROKEN_AT=$(date -u +%FT%TZ)
    sleep 120
    FAILED=$(sql "select failed_count from pg_stat_archiver")
    FAULT="{\"applied\":\"archive credentials replaced at $BROKEN_AT\",\"archiver_failed_count_after_120s\":$FAILED}" ;;
  *) echo "unknown scenario $SCEN" >&2; exit 2 ;;
esac
log "fault: $FAULT"

T_CHECK=$(now)
CNPG_BACKUPS_BEFORE=$(cnpg_view)
if [ "$CHECKER" = cronjob ]; then
  kubectl -n "$NS" apply -f "$HERE/manifests/baseline-cronjob.yaml" >/dev/null
  kubectl -n "$NS" create job restore-test-run --from=cronjob/restore-test >/dev/null
  for i in $(seq 1 330); do
    st=$(kubectl -n "$NS" get job restore-test-run -o jsonpath='{.status.succeeded}{.status.failed}')
    [ -n "$st" ] && break
    sleep 5
  done
else
  check_run "select count(*) = $ROWS from orders" 60s 2Gi
fi
T_DONE=$(now)
hb_stop
trap - EXIT

P="$OUT/.parts-${SCEN}-${REP}"; mkdir -p "$P"
echo "$FAULT" > "$P/fault.json"
echo "$CNPG_BACKUPS_BEFORE" > "$P/cnpg_backups_before_check.json"
cnpg_view > "$P/cnpg_backups_at_verdict.json"
cnpg_conditions > "$P/cnpg_cluster_at_verdict.json"
if [ "$CHECKER" = cronjob ]; then
  kubectl -n "$NS" get job restore-test-run -o json | python3 -c 'import json,sys; j=json.load(sys.stdin); s=j.get("status",{}); print(json.dumps({"succeeded":s.get("succeeded",0),"failed":s.get("failed",0),"startTime":s.get("startTime"),"completionTime":s.get("completionTime"),"conditions":[{"type":c["type"],"reason":c.get("reason"),"time":c.get("lastTransitionTime")} for c in s.get("conditions",[])]}))' > "$P/cronjob.json"
  kubectl -n "$NS" logs job/restore-test-run --tail=20 2>&1 | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()[-2000:]))' > "$P/cronjob_log.json"
  sleep 20
else
  rcheck_view > "$P/restorecheck.json"
fi
leftovers > "$P/leftovers_after_run.json"
python3 "$HERE/lib/merge.py" "$P" "$OUT/${SCEN}-${REP}.json"   suite="$([ "$CHECKER" = cronjob ] && echo baseline || echo detection)" checker="$CHECKER" run_id="$RUNID" scenario="$SCEN" rep="$REP" namespace="$NS" postgres="$PGVERSION"   data_checksums="$CHECKSUMS" orders_relpath="$RELPATH"   seconds_setup_and_fault=$((T_CHECK - T0)) seconds_check=$((T_DONE - T_CHECK))
s3op delete-prefix "$RUNID/" >/dev/null || log "WARN: could not delete bucket prefix $RUNID/"
ns_delete
