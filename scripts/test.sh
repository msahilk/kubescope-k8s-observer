#!/usr/bin/env bash
# Run all automated checks that do not need a cluster.
set -euo pipefail
cd "$(dirname "$0")/.."
echo "== backend: gofmt, vet, tests (race detector)"
(cd backend && test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...)
echo "== frontend: typecheck + production build"
(cd frontend && npm run build)
echo "All checks passed."
