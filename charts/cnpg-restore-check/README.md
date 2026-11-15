# cnpg-restore-check

Scheduled restore checks for CloudNativePG backups made with plugin-barman-cloud.
A `RestoreCheck` restores your real backup on a schedule into a temporary
cluster, runs amcheck, SQL assertions and a data-freshness check, measures
recovery time (RTO) and data age (RPO), and alerts when a backup will not
actually restore.

```sh
helm install cnpg-restore-check oci://ghcr.io/shanurwan/charts/cnpg-restore-check   --namespace cnpg-restore-check --create-namespace
```

Requires CloudNativePG 1.26+ with plugin-barman-cloud.

- [Quickstart on kind](https://github.com/shanurwan/cnpg-restore-check/blob/main/docs/quickstart.md)
- [Configuration reference and safety model](https://github.com/shanurwan/cnpg-restore-check#readme)
- [Alert runbooks](https://github.com/shanurwan/cnpg-restore-check/blob/main/docs/runbooks.md)

Main values: `maxConcurrentRuns` (default 1), `watchNamespaces`,
`metrics.serviceMonitor.enabled`, `prometheusRule.enabled`,
`metrics.service.annotations`.
