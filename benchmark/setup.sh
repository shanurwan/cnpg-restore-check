#!/usr/bin/env bash
# Installs everything the benchmark needs on an empty Kubernetes cluster.
# Tested on a single-node K3s v1.35.10 (Ubuntu 24.04 VM, 4 vCPU, 8 GiB, local-path storage).
set -euo pipefail
cd "$(dirname "$0")"
. ./versions.env

kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER}/cert-manager.yaml"
kubectl -n cert-manager rollout status deploy --timeout=5m

kubectl apply --server-side -f "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG}/cnpg-${CNPG}.yaml"
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=5m

kubectl apply -f "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/${BARMAN_CLOUD}/manifest.yaml"
kubectl -n cnpg-system rollout status deploy/barman-cloud --timeout=5m

kubectl create namespace cnpg-restore-check --dry-run=client -o yaml | kubectl apply -f -
kubectl apply --server-side -f "https://github.com/shanurwan/cnpg-restore-check/releases/download/${RESTORE_CHECK}/install.yaml"
kubectl -n cnpg-restore-check rollout status deploy --timeout=5m

# One S3 server (RustFS) shared by every scenario; each scenario writes under its own prefix.
kubectl create namespace bench-s3 --dry-run=client -o yaml | kubectl apply -f -
kubectl -n bench-s3 apply -f ../examples/quickstart/s3.yaml
kubectl -n bench-s3 rollout status deploy/s3 --timeout=5m

# A long-lived pod with boto3, used to inspect and damage backups in the bucket.
kubectl -n bench-s3 apply -f manifests/s3tool.yaml
kubectl -n bench-s3 wait pod/s3tool --for=condition=Ready --timeout=5m
kubectl -n bench-s3 exec s3tool -- sh -c 'until python -c "import boto3" 2>/dev/null; do sleep 2; done'
kubectl -n bench-s3 cp lib/s3ops.py s3tool:/tmp/s3ops.py
kubectl -n bench-s3 exec s3tool -- python /tmp/s3ops.py mkbucket
echo "environment ready"
