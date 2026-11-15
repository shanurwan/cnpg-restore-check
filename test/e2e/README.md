# End-to-end tests

The suite runs against the current `kubectl` context and creates everything it
needs in a fresh namespace `crc-e2e-<n>`: an S3 server, two ObjectStores, a
source cluster that archives WAL, base backups, and the RestoreChecks below.

Prerequisites in the cluster:

- CloudNativePG 1.26+ and plugin-barman-cloud (which needs cert-manager)
- the cnpg-restore-check controller, installed in namespace `cnpg-restore-check`
  (override with `CRC_NAMESPACE`; the metrics service name with `CRC_METRICS_SERVICE`)
- a default StorageClass

```sh
make e2e
# keep the namespace when a test fails:
CRC_E2E_KEEP=1 make e2e
```

| Test | Asserts |
| --- | --- |
| refuses the object store the source archives to | `InvalidSpec` naming the source cluster; no cluster created |
| healthy backup passes | `Succeeded`; amcheck on every database, assertion and freshness pass; recovery time and data age measured; temporary cluster deleted; `last_run_success` exported |
| missing WAL segment | one archived segment deleted from the bucket; the restore reaches Ready, but the run fails with `ObjectiveMissed` and a data age of about an hour |
| quiet database, standby backup (plugin-barman-cloud#652) | the backup ends inside a WAL segment the idle primary has not archived; the restore misses a 3m recovery objective, then passes after `pg_switch_wal()` |
| unknown server name | `RecoveryFailed` |

A full run takes about 25 minutes, most of it the two scenarios that wait for CloudNativePG's recovery job to retry.
