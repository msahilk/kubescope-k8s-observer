#!/usr/bin/env bash
# Create a kubeconfig that authenticates as the least-privilege "kubescope"
# ServiceAccount from k8s/optional-rbac.yaml, using a short-lived token.
# Usage: ./scripts/make-restricted-kubeconfig.sh <source-context> [out-file]
# Then:  KUBECONFIG=kubescope-restricted.kubeconfig KUBESCOPE_CONTEXT=kubescope-restricted ...
# The output file contains a bearer token: never commit it (it is gitignored).
set -euo pipefail
cd "$(dirname "$0")/.."
SRC="${1:?usage: $0 <source-context> [out-file]}"
OUT="${2:-kubescope-restricted.kubeconfig}"

kubectl --context "$SRC" apply -f k8s/optional-rbac.yaml
TOKEN=$(kubectl --context "$SRC" -n kubescope-demo create token kubescope --duration=8h)
CLUSTER=$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$SRC\")].context.cluster}")
SERVER=$(kubectl config view -o jsonpath="{.clusters[?(@.name==\"$CLUSTER\")].cluster.server}")
CA=$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name==\"$CLUSTER\")].cluster.certificate-authority-data}")

umask 077
cat > "$OUT" <<YAML
apiVersion: v1
kind: Config
clusters:
- name: $CLUSTER
  cluster:
    server: $SERVER
    certificate-authority-data: $CA
users:
- name: kubescope-sa
  user:
    token: $TOKEN
contexts:
- name: kubescope-restricted
  context: {cluster: $CLUSTER, user: kubescope-sa, namespace: kubescope-demo}
current-context: kubescope-restricted
YAML
echo "Wrote $OUT (token valid 8h). Verify with:"
echo "  KUBECONFIG=$OUT kubectl auth can-i patch deployments/demo-api -n kubescope-demo   # yes"
echo "  KUBECONFIG=$OUT kubectl auth can-i delete pods -n default                         # no"
