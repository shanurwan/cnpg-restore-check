# Quickstart on kind

This sets up a laptop cluster with CloudNativePG, a database that archives to a
local S3 server, and a RestoreCheck. Then it breaks the backup and shows the
check catching it. It takes about 10 minutes, most of it waiting for images.

You need `docker`, [`kind`](https://kind.sigs.k8s.io), `kubectl` and `helm`.

## 1. Cluster and operators

```sh
kind create cluster --name restore-check

kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl -n cert-manager rollout status deploy --timeout=5m

kubectl apply --server-side -f https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=5m

kubectl apply -f https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.16.0/manifest.yaml
kubectl -n cnpg-system rollout status deploy/barman-cloud --timeout=5m

helm install cnpg-restore-check oci://ghcr.io/shanurwan/charts/cnpg-restore-check \
  --namespace cnpg-restore-check --create-namespace --wait
```

## 2. A database with backups

```sh
git clone https://github.com/shanurwan/cnpg-restore-check && cd cnpg-restore-check/examples/quickstart
kubectl create namespace shop
kubectl config set-context --current --namespace=shop

kubectl apply -f s3.yaml
kubectl rollout status deploy/s3 --timeout=5m
kubectl run mkbucket --rm -i --restart=Never --image=amazon/aws-cli:2.31.13 \
  --env AWS_ACCESS_KEY_ID=quickstart --env AWS_SECRET_ACCESS_KEY=quickstart-secret --env AWS_DEFAULT_REGION=us-east-1 \
  -- --endpoint-url http://s3:9000 s3api create-bucket --bucket backups

kubectl apply -f database.yaml
kubectl wait cluster/shop --for=condition=Ready --timeout=10m
```

Add some data and a heartbeat table. In a real database, a CronJob writes the
heartbeat every minute ([examples/heartbeat.yaml](../examples/heartbeat.yaml)).

```sh
psql() { kubectl exec -i shop-1 -c postgres -- psql -q -d app -tA "$@"; }
psql -c "create table orders(id bigserial primary key, note text);
         insert into orders(note) select md5(i::text) from generate_series(1, 1000) i;
         create table heartbeat(ts timestamptz not null default now());
         insert into heartbeat default values;"

kubectl apply -f backup.yaml
kubectl wait backup/shop-base --for=jsonpath='{.status.phase}'=completed --timeout=10m

# a write after the backup, and a WAL switch so it reaches the archive now
psql -c "insert into heartbeat default values; select pg_switch_wal();"
```

## 3. Check the backup

```sh
kubectl apply -f restorecheck.yaml
kubectl get restorecheck shop -w
```

A new check runs right away, then on its schedule. About a minute later:

```console
NAME   SCHEDULE    PHASE   LAST RESULT   REASON   RECOVERY   DATA AGE   LAST SUCCESS
shop   0 * * * *           Succeeded              55s        11s        0s
```

`kubectl describe restorecheck shop` shows each check, and the events. The
temporary cluster (`shop-<timestamp>`) is already deleted.

## 4. Break the backup

A missing WAL segment is the failure that hides best: recovery to the latest
point stops at the gap and promotes, with no error. To see it, write a row dated
an hour ago (it stands in for an hour of history), switch WAL twice, and delete
the middle segment from the bucket:

```sh
psql -c "create table probe(ts timestamptz);
         insert into probe values (now() - interval '1 hour')"
psql -c "select pg_switch_wal()"
LOST=$(psql -c "insert into probe values (now()); select pg_walfile_name(pg_current_wal_insert_lsn())" | tail -1)
psql -c "select pg_switch_wal()"
psql -c "insert into probe values (now()); select pg_switch_wal()"
sleep 15   # let the archiver upload

kubectl run rmwal --rm -i --restart=Never --image=amazon/aws-cli:2.31.13 \
  --env AWS_ACCESS_KEY_ID=quickstart --env AWS_SECRET_ACCESS_KEY=quickstart-secret --env AWS_DEFAULT_REGION=us-east-1 \
  -- --endpoint-url http://s3:9000 s3 rm --recursive "s3://backups/shop/wals/${LOST:0:16}/" --exclude "*" --include "${LOST}*"
```

Point the freshness check at the probe table and run it now:

```sh
kubectl patch restorecheck shop --type merge \
  -p '{"spec":{"checks":{"freshness":{"database":"app","query":"select max(ts) from probe"}}}}'
kubectl annotate restorecheck shop --overwrite restorecheck.shanurwan.github.io/run-now="$(date +%s)"
kubectl get restorecheck shop -w
```

```console
NAME   SCHEDULE    PHASE   LAST RESULT   REASON            RECOVERY   DATA AGE   LAST SUCCESS
shop   0 * * * *           Failed        ObjectiveMissed   57s        1h0m18s    76s
```

The restored cluster came up Ready, and every other check passed. Only the data
age shows that a real restore would have lost the last hour.

## Clean up

```sh
kind delete cluster --name restore-check
```
