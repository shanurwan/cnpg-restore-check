# Design

## Problem

A CloudNativePG backup is only proven by restoring it. `Backup` objects and the
ObjectStore's status say that a base backup was uploaded and that WAL archiving
is working. They do not say that the backup can be restored, how long that takes,
or how much data a restore would bring back.

Restores fail for reasons that never show up as a failed backup:

- A WAL segment is missing from the archive (deleted by a lifecycle rule,
  lost in a bucket migration, never uploaded). Recovery to the latest point
  stops at the gap and promotes **without an error**. The restore looks healthy
  and silently lacks everything after the gap.
- The base backup needs WAL that is not archived yet, for example on a quiet
  database ([plugin-barman-cloud#652](https://github.com/cloudnative-pg/plugin-barman-cloud/issues/652)).
- Recovery crosses a timeline switch after a failover and fails
  ([cloudnative-pg#10422](https://github.com/cloudnative-pg/cloudnative-pg/issues/10422)).
- The restore settings do not match the source (`max_connections` lower than
  the source's stops recovery), or parallel WAL restore misbehaves
  ([plugin-barman-cloud#516](https://github.com/cloudnative-pg/plugin-barman-cloud/issues/516)).
- Credentials, bucket policies or the plugin changed since the last time anyone
  restored.

Teams usually learn about these during an incident. The usual workaround is a
CronJob script per database that is rarely maintained or monitored.

## Goals

1. Restore real backups on a schedule, the same way a real recovery would.
2. Measure recovery time (RTO) and the age of the newest restored data (RPO)
   on every run, and fail runs that miss stated objectives.
3. Verify the restored data: physical integrity (amcheck) and user assertions.
4. Never put the source cluster or its backups at risk.
5. Fit GitOps: checks are declarative objects; results are in status, events
   and Prometheus metrics, with alerts and runbooks.

Non-goals for v0.1: in-tree `barmanObjectStore` (deprecated), volume snapshot
backups, logical (`pg_dump`) verification, and restoring into another
Kubernetes cluster. Those are possible later; the run model does not depend on
the backup method.

## Approach

A controller watches `RestoreCheck` objects. Each run:

1. **Validates**: the ObjectStore exists; the spec is consistent; the
   ObjectStore is not one that a Cluster in the namespace writes to (see Safety).
2. **Restores**: creates a one-instance CloudNativePG `Cluster` bootstrapped
   with `recovery` from the ObjectStore through plugin-barman-cloud, owned by
   the RestoreCheck. Recovery time is measured from creating that Cluster until
   its `Ready` condition is true.
3. **Checks**: connects to the restored primary with its superuser secret and
   runs, in order: connectivity, amcheck on every database (B-tree indexes,
   optionally heap pages and heap-to-index consistency), SQL assertions, and a
   freshness query that returns the newest timestamp in the data.
4. **Evaluates**: a run fails on a failed check, a missed objective, a failed
   recovery job, or the timeout.
5. **Records and cleans up**: writes the result to `status.lastRun` and
   `status.history`, emits an Event, updates metrics, and deletes the cluster
   (kept on failure if `keepFailedClusters` is set, until the next run).

The controller restores into a real CloudNativePG cluster on purpose, rather
than running `barman-cloud-restore` itself. A check that uses a different code
path from a real recovery would miss failures in the operator, the plugin, the
image and the cluster settings, which are part of what a real recovery depends on.

### Data age (RPO) instead of "replay succeeded"

Recovery to the latest point has no notion of "should have reached X": it
replays whatever WAL it finds and stops. The only reliable way to see how far a
restore got is to look at the data. The freshness check runs a user query such
as `SELECT max(ts) FROM heartbeat` and reports `run start - result`. With a
heartbeat table written every minute, this is the real recovery point of the
backup, including archiving lag and gaps. The e2e suite deletes one archived WAL
segment and shows that the restore still comes up Ready while the freshness check
reports data about an hour old.

### Why one controller and a CRD, not a CronJob

- Each run creates and waits on Kubernetes objects (Cluster, Jobs, Secrets).
  A reconcile loop handles controller restarts and timeouts and cleans up after
  itself without bespoke scripting.
- Results belong in status and metrics, not in pod logs that expire.
- One installation serves every database. Teams add a `RestoreCheck` next to
  their `Cluster` in Git.

## Safety

The main risk of a restore test is writing into the backups it is testing.

- **No archiving from restored clusters.** The temporary Cluster never gets
  `spec.plugins` or `spec.backup`. CloudNativePG therefore never configures WAL
  archiving or backups for it. Unit tests assert this on the built object.
- **Separate, read-only ObjectStore.** The RestoreCheck names an ObjectStore in
  its own namespace. The controller refuses to run if a Cluster in that
  namespace references the same ObjectStore in `spec.plugins` (it is a write
  path), unless `allowSharedObjectStore` is set. The recommended setup gives that
  ObjectStore credentials that can only read the bucket (IAM read-only role,
  GCS `roles/storage.objectViewer` through Workload Identity, or an Azure SAS
  with read and list only).
- **The source cluster is never touched.** The controller does not read or
  modify the source Cluster; it only needs the bucket.
- **Least privilege.** The controller can create and delete only CloudNativePG
  Clusters, and read only Secrets, Jobs and ObjectStores, without caching
  Secrets. `--watch-namespaces` limits it to chosen namespaces.
- **Bounded cost.** One instance per run, a run timeout (default 2h),
  `--max-concurrent-runs` across all checks (default 1), and deletion of the
  cluster at the end of the run.
- **Credentials of restored data.** A restored cluster contains production data.
  Run checks in a namespace with the same access controls as the source, and
  apply NetworkPolicies there that allow only the controller to reach port 5432.

## Failure modes of the controller itself

| Situation | Behaviour |
| --- | --- |
| Controller restarts mid-run | State is in `status.currentRun`; the run continues. Gauges are rebuilt from status on the first reconcile. |
| Temporary Cluster deleted by someone | Run fails with `ClusterLost`. |
| Recovery job fails | Detected from the Job's `Failed` condition; run fails with `RecoveryFailed` and the command to read the job's logs. |
| Recovery hangs (missing WAL that blocks consistency, image pull) | Run fails with `Timeout` and the cluster phase. |
| Several runs due at once | Started one by one up to `--max-concurrent-runs`; others wait. |
| Controller down for days | On return, one run starts for missed slots, not one per slot. `CNPGRestoreCheckStale` fires when `successAge` passes. |
| Run stuck in progress | `CNPGRestoreCheckRunStuck` fires after 6h. |

## Alternatives considered

- **CronJob with a script.** The common practice today. Works for one database;
  no common result format, metrics, objectives or safety checks.
- **EDB's Klio `backup verify`.** Commercial and documented as a tech preview.
- **A CNPG-I plugin.** Plugins hook into a Cluster's lifecycle; a restore check is
  a separate workload with its own schedule and lifecycle, so a controller next to
  the operator fits better. It could become part of the CloudNativePG project
  later if maintainers want that.
- **Restore into another Kubernetes cluster.** Stronger isolation, much more
  setup. A later option: the controller could create the Cluster through a
  kubeconfig for a remote cluster.

## Metrics

| Metric | Meaning |
| --- | --- |
| `cnpg_restore_check_last_run_success` | 1 if the last run succeeded |
| `cnpg_restore_check_last_run_timestamp_seconds` | completion time of the last run |
| `cnpg_restore_check_last_success_timestamp_seconds` | completion time of the last success |
| `cnpg_restore_check_recovery_duration_seconds` | measured recovery time (last run that reached Ready) |
| `cnpg_restore_check_data_age_seconds` | measured data age (last run with a freshness check) |
| `cnpg_restore_check_run_in_progress` | 1 while a run is in progress |
| `cnpg_restore_check_runs_total{result,reason}` | finished runs |
| `cnpg_restore_check_objective_*_seconds` | the objectives, so alerts compare against per-check values |

All have `namespace` and `name` labels. Exporting objectives as gauges lets one
set of alert rules serve every RestoreCheck with its own objectives.
