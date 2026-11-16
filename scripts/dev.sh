#!/usr/bin/env bash
# Start the Go backend (127.0.0.1:8080) and the Vite dev server (127.0.0.1:5173).
# Usage: KUBESCOPE_CONTEXT=kind-kubescope ./scripts/dev.sh
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -f .env ]]; then
  set -a; source .env; set +a
fi
: "${KUBESCOPE_CONTEXT:=kind-kubescope}"
export KUBESCOPE_CONTEXT
echo "Using kubeconfig context: ${KUBESCOPE_CONTEXT}"
case "$KUBESCOPE_CONTEXT" in
  gke_*) echo "WARNING: this is a GKE context. Experiments will change a real, billable cluster." ;;
esac

if [[ ! -f backend/go.sum ]]; then
  echo "backend/go.sum missing: resolving module checksums (one-time, needs network)…"
  (cd backend && go mod tidy)
fi
if [[ ! -d frontend/node_modules ]]; then
  echo "Installing frontend dependencies (one-time)…"
  (cd frontend && npm install)
fi

(cd backend && go run ./cmd/server) &
BACKEND_PID=$!
trap 'kill $BACKEND_PID 2>/dev/null || true' EXIT INT TERM

for _ in $(seq 1 60); do
  curl -fsS http://127.0.0.1:8080/api/v1/healthz >/dev/null 2>&1 && break
  sleep 1
done

echo "Open http://127.0.0.1:5173"
cd frontend && npm run dev
