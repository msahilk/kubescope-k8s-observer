#!/usr/bin/env bash
# End-to-end smoke test against a RUNNING KubeScope backend connected to a real
# cluster (kind by default). It performs real experiments:
#   scale 2 -> 4, scale 4 -> 2, delete one demo Pod, then exports history.
# Usage: ./scripts/smoke-test.sh [http://127.0.0.1:8080]
# Requires: curl, python3. Refuses to run against a GKE context unless
# KUBESCOPE_SMOKE_ALLOW_GKE=yes is set.
set -euo pipefail
API="${1:-http://127.0.0.1:8080}/api/v1"
OUT="${SMOKE_OUT:-smoke-results.json}"

json() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {'d': d}))" "$1"; }
post() { curl -sS -X POST -H 'Content-Type: application/json' -d "$2" "$API$1"; }

fail() { echo "FAIL: $*" >&2; exit 1; }

curl -fsS "$API/healthz" >/dev/null || fail "backend not reachable at $API"
CONN=$(curl -fsS "$API/connection")
echo "context=$(echo "$CONN" | json "d['context']") reachable=$(echo "$CONN" | json "d['reachable']") gke=$(echo "$CONN" | json "d['isGKE']")"
[[ "$(echo "$CONN" | json "d['reachable']")" == "True" ]] || fail "cluster not reachable: $(echo "$CONN" | json "d.get('error','')")"
if [[ "$(echo "$CONN" | json "d['isGKE']")" == "True" && "${KUBESCOPE_SMOKE_ALLOW_GKE:-}" != "yes" ]]; then
  fail "refusing to run experiments on GKE without KUBESCOPE_SMOKE_ALLOW_GKE=yes"
fi

SNAP=$(curl -fsS "$API/snapshot")
echo "snapshot: desired=$(echo "$SNAP" | json "d['deployment']['desired']") ready=$(echo "$SNAP" | json "d['deployment']['ready']") pods=$(echo "$SNAP" | json "len(d['pods'])")"

wait_done() {
  local id=$1 status
  for _ in $(seq 1 150); do
    status=$(curl -fsS "$API/experiments/$id" | json "d['status']")
    [[ "$status" != "running" ]] && { echo "$status"; return; }
    sleep 1
  done
  echo "still-running"
}

report() {
  EXP_JSON="$(curl -fsS "$API/experiments/$1")" python3 - <<'PY'
import json, os
d = json.loads(os.environ["EXP_JSON"])
print(f"  {d['id']} {d['type']} status={d['status']} ({d.get('statusReason', '')})")
for m in d["measurements"]:
    v = f"n/a ({m.get('reason', '')})" if m["valueMs"] is None else f"{m['valueMs']} ms"
    print(f"    {m['name']:<30} {v}")
PY
}

run_scale() {
  local n=$1 resp id st
  resp=$(post /experiments/scale "{\"deployment\":\"demo-api\",\"replicas\":$n}")
  id=$(echo "$resp" | json "d.get('id','')") || true
  [[ -n "$id" ]] || fail "scale to $n rejected: $resp"
  st=$(wait_done "$id")
  report "$id"
  [[ "$st" == "success" ]] || fail "scale to $n ended with $st"
}

echo "== scale 2 -> 4"; run_scale 4
echo "== scale 4 -> 2"; run_scale 2

echo "== delete one Pod"
POD=$(curl -fsS "$API/snapshot" | json "[p['name'] for p in d['pods'] if p['ready'] and not p['terminating']][0]")
resp=$(post /experiments/delete-pod "{\"podName\":\"$POD\",\"confirm\":true}")
id=$(echo "$resp" | json "d.get('id','')") || true
[[ -n "$id" ]] || fail "delete-pod rejected: $resp"
st=$(wait_done "$id")
report "$id"
[[ "$st" == "success" ]] || fail "pod recovery ended with $st"
NEWUID=$(curl -fsS "$API/experiments/$id" | json "d['target'].get('replacementUid','')")
OLDUID=$(curl -fsS "$API/experiments/$id" | json "d['target']['podUid']")
[[ -n "$NEWUID" && "$NEWUID" != "$OLDUID" ]] || fail "replacement UID not observed"
echo "  replaced uid $OLDUID -> $NEWUID"

echo "== negative checks"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"deployment":"demo-api","replicas":99}' "$API/experiments/scale")
[[ "$code" == "400" ]] || fail "out-of-range replicas returned $code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H 'Origin: https://evil.example' -d '{"replicas":3}' "$API/experiments/scale")
[[ "$code" == "403" ]] || fail "cross-origin mutation returned $code"
echo "  validation and origin checks OK"

curl -fsS "$API/experiments/export" > "$OUT"
echo "PASS — results exported to $OUT"
