# Runbooks

Each alert links here. Start with the same two commands:

```sh
kubectl describe restorecheck -n <namespace> <name>      # status, last run, events
kubectl get restorecheck -n <namespace> <name> -o jsonpath='{.status.lastRun}' | jq
```

`status.lastRun.reason` tells you which section below applies. To re-run after a
fix without waiting for the schedule:

```sh
kubectl annotate restorecheck -n <namespace> <name> --overwrite \
  restorecheck.shanurwan.github.io/run-now="$(date +%s)"
```

Set `spec.keepFailedClusters: true` while debugging, so the restored cluster
stays after a failed run (it is deleted when the next run starts).

---

## CNPGRestoreCheckFailed

**Meaning:** the last restore of this backup failed or missed an objective. Treat
the backup as possibly not restorable until a run succeeds. Severity is critical
because the failure stays hidden until someone needs the backup.

**Triage by `status.lastRun.reason`:**

- `RecoveryFailed`: the CloudNativePG recovery job failed. Read its logs (the
  message contains the exact command):
  ```sh
  kubectl logs -n <namespace> job/<cluster>-1-full-recovery
  ```
  Common causes:
  - wrong `serverName`, or no base backup under it;
  - the ObjectStore credentials cannot read the bucket (`AccessDenied`, `403`);
  - the image's PostgreSQL major version differs from the source's;
  - `max_connections`, `max_worker_processes` or similar settings are lower than
    the source's. Set them in `spec.cluster.parameters`.
- `Timeout`: the run did not finish within `spec.timeout`. The message gives the
  cluster phase. Check:
  ```sh
  kubectl get cluster -n <namespace> <cluster> -o yaml | yq .status
  kubectl get pods -n <namespace> -l cnpg.io/cluster=<cluster>
  kubectl logs -n <namespace> <cluster>-1 -c postgres | tail -100
  ```
  Recovery that waits for a WAL file it can never get, image pulls, PVC
  provisioning and slow restores of large backups all end up here. For large
  databases, raise `spec.timeout` and compare with your RTO.
- `CheckFailed`: the restore worked, but a check did not. `status.lastRun.checks`
  lists each check with its message.
  - `amcheck/<db>`: index or heap corruption in the backup. Run the same check on
    the source primary (`SELECT bt_index_check(...)`) to find out whether the
    source is corrupt or only the backup is. Escalate to the database owner; take
    a new base backup once the source is clean.
  - `sql/<name>`: an assertion returned false or errored. Check whether the
    assertion is still valid (schema changes) before assuming data loss.
  - `freshness`: the query errored or returned NULL (the table is empty or does
    not exist in the restored data).
- `ObjectiveMissed`: see the two objective alerts below.
- `InvalidSpec`: the message says what to fix, typically a missing ObjectStore,
  an ObjectStore that a Cluster also writes to, or `objectives.dataAge` without
  `checks.freshness`.
- `ClusterLost`: someone or something deleted the restored cluster during the
  run. Look for namespace cleanup jobs or policies that delete unknown Clusters.

**Escalation:** if the backup cannot be restored and the cause is not
configuration, tell the owner of the source database now. Their recovery point
in an incident is the last backup that *did* restore.

## CNPGRestoreCheckDataAgeAboveObjective

**Meaning:** the newest data in the restored cluster was older than
`spec.objectives.dataAge` when the run started. A real restore would lose at
least that much data.

**Triage:**

1. Is WAL archiving on the source working? On the source primary:
   ```sql
   SELECT last_archived_wal, last_archived_time, last_failed_wal, last_failed_time
   FROM pg_stat_archiver;
   ```
   and the source Cluster's `ContinuousArchiving` condition. Failing archiving
   means the source is accumulating WAL locally; check its disk too.
2. Is the heartbeat (or whatever the freshness query reads) still being written
   on the source? A stopped writer looks like data loss.
3. Is there a gap in the archive? Recovery to the latest point stops silently at
   the first missing segment. Compare the segments in the bucket with the
   source's archived WAL; look for lifecycle rules, retention policies or
   manual deletions on the bucket.
4. Quiet databases: if nothing is written, a segment is only archived when it
   fills or `archive_timeout` passes. Make sure `archive_timeout` is lower than
   your objective.

**Mitigation:** fix archiving, then take a new base backup so the next run does
not depend on the gap.

## CNPGRestoreCheckRecoveryTimeAboveObjective

**Meaning:** restoring took longer than `spec.objectives.recoveryTime`. In an
incident, recovery would take at least this long. This is a capacity and
planning problem, not a broken backup.

**Triage:**

- Compare `recoveryDuration` across `status.history`: steady growth follows
  data size; a jump points to a change (storage class, node type, bucket
  location, throttling).
- Read the recovery job's logs for time spent downloading the base backup
  versus replaying WAL. A lot of WAL since the last base backup means more
  frequent base backups shorten recovery.
- Check that the restore runs on hardware like what a real recovery would use;
  undersized restore nodes make the measurement pessimistic.

**Mitigation:** more frequent base backups, faster storage, parallel WAL
restore, or, if the objective is unrealistic, agree a new one with the owner.

## CNPGRestoreCheckStale

**Meaning:** no run has succeeded for longer than `spec.objectives.successAge`.
Either runs are failing (see `CNPGRestoreCheckFailed`), or they are not running.

**Triage:**

- `spec.suspend` set? `spec.schedule` still valid
  (`status.conditions` shows `InvalidSpec`)?
- Is the controller running and reconciling?
  ```sh
  kubectl -n cnpg-restore-check logs deploy/cnpg-restore-check --tail=100
  ```
- Are runs waiting for the concurrency limit? The controller logs
  `concurrency limit is reached`. Raise `maxConcurrentRuns` or spread schedules.

## CNPGRestoreCheckRunStuck

**Meaning:** a run has been in progress for over 6 hours, beyond any normal
`spec.timeout`. The controller is probably not reconciling this object.

**Triage:**

- Controller pod running and not crash-looping? Leader election stuck (with
  `leaderElection.enabled`)?
- Controller logs for errors updating this RestoreCheck (RBAC, admission
  webhooks rejecting status updates).
- `status.currentRun.clusterName`: does that Cluster still exist?

**Mitigation:** fix the controller. As a last resort, delete the temporary
Cluster; the next reconcile records `ClusterLost` and the schedule continues.

---

## Handoff note template

Use this when handing an open restore-check alert to the next on-call region.

```
Alert:        <alert> for <namespace>/<name>, firing since <time UTC>
Last run:     <result>/<reason> at <time>, recovery <duration>, data age <age>
Impact:       <which database; is the latest restorable backup older than the objective?>
Done so far:  <what was checked, commands run, findings>
Current idea: <suspected cause>
Next step:    <the one thing the next person should do first>
Kept cluster: <name, if keepFailedClusters is on; delete when done>
Owner told:   <yes/no, who>
```
