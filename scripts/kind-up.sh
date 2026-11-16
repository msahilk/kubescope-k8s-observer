#!/usr/bin/env bash
# Create (or reuse) the local kind cluster and deploy the demo workload.
# Requires: docker (running), kind, kubectl.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=kubescope
CTX="kind-${CLUSTER}"

for bin in docker kind kubectl; do
  command -v "$bin" >/dev/null || { echo "error: '$bin' not found on PATH" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "error: Docker is not running" >&2; exit 1; }

if kind get clusters | grep -qx "$CLUSTER"; then
  echo "kind cluster '$CLUSTER' already exists"
else
  kind create cluster --config k8s/kind-config.yaml
fi

kubectl --context "$CTX" apply -f k8s/demo-namespace.yaml
kubectl --context "$CTX" apply -f k8s/demo-deployment.yaml
kubectl --context "$CTX" -n kubescope-demo rollout status deployment/demo-api --timeout=180s
kubectl --context "$CTX" -n kubescope-demo get deployment,replicaset,pods -o wide
echo
echo "Ready. Start KubeScope with:  KUBESCOPE_CONTEXT=$CTX ./scripts/dev.sh"
