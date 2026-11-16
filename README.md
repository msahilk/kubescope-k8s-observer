# KubeScope

KubeScope is a small Go + React tool for **watching the Kubernetes control plane reconcile desired state**. You scale a demo Deployment or delete one of its Pods, and KubeScope shows the API request, the watch events, Kubernetes Events and the moment the Deployment is actually back to ready. It keeps *API round-trip time* separate from *time to readiness*.

It is a **client of the standard Kubernetes API**. It is not a control plane, and it cannot see inside GKE's managed control plane. Every timestamp is taken on your machine.

```
Browser (React, :5173) ──REST + SSE──▶ Go backend (127.0.0.1:8080) ──client-go──▶ kube-apiserver (kind or GKE)
```

> **Status:** verified end-to-end on a **real local kind cluster** (macOS, Apple Silicon, Colima): automated tests, production build, the smoke test (4 passing runs, plus 1 with a least-privilege ServiceAccount), and outage/recovery checks. **GKE has not been tested.** See [IMPLEMENTATION_NOTES.md](IMPLEMENTATION_NOTES.md) and the screenshots in [docs/screenshots](docs/screenshots/).

## Prerequisites

| Tool | Version | Used for |
|---|---|---|
| Go | 1.22+ | backend |
| Node.js + npm | 20+ | dashboard |
| Docker, kind, kubectl | recent | local cluster |
| gcloud + `gke-gcloud-auth-plugin` | optional | GKE ([tutorial](GKE_SETUP_TUTORIAL.html)) |

On macOS you don't need Docker Desktop. With [Homebrew](https://brew.sh/):

```bash
brew install go node kubectl kind colima docker
colima start --cpu 2 --memory 4    # Docker engine in a small VM; run again after a reboot
docker info                        # should print "Server Version: …"
```

## Quick start (local kind cluster)

```bash
./scripts/kind-up.sh                                # creates kind cluster "kubescope", deploys k8s/demo-*.yaml
KUBESCOPE_CONTEXT=kind-kubescope ./scripts/dev.sh   # backend on :8080 + Vite on :5173
# open http://127.0.0.1:5173
```

To run the two halves yourself:

```bash
# terminal 1
cd backend
go mod tidy                                    # first run only: writes go.sum (see note below)
KUBESCOPE_CONTEXT=kind-kubescope go run ./cmd/server

# terminal 2
cd frontend
npm install                                    # first run only
npm run dev
```

With the backend running, `./scripts/smoke-test.sh` runs scale 2→4, 4→2 and a Pod deletion through the API, prints every measurement, runs two negative checks and exports the backend's whole experiment history to `smoke-results.json` (set `SMOKE_OUT=…` to choose the file). Results from the real kind runs are in [docs/results](docs/results/).

`backend/go.sum` and `frontend/package-lock.json` are committed, so `go mod tidy` and `npm install` just reproduce the pinned versions.

## Using the dashboard

- **Connection**: shows context, cluster, API server, namespace, reachability, last successful refresh, live-stream state and watch errors. For a GKE context it shows a GKE badge and a cost warning. If the cluster can't be reached, KubeScope shows an error, never an empty cluster.
- **Demo Deployment**: desired / current / updated / ready / available counts, convergence state, and the Pod table. *Running* and *Ready* are separate columns.
- **Experiments**: *Scale* (1–5 replicas by default), *Reset to baseline (2)…* (asks for confirmation), *Cancel running experiment*. To delete a Pod, click *Delete…* on its row and confirm.
- **Current experiment**: status, measurements (each with "What does this measure?" start/end conditions), a timestamped timeline in which **Observed** entries are marked apart from KubeScope's **Explanation** entries, caveats, and *Export JSON*.
- **Live cluster feed**: raw watch changes and Kubernetes Events.
- **History**: the last 20 runs, kept in backend memory. *Export all JSON* downloads them.
- **Learn**: a short glossary, plus links to [LEARNING_GUIDE.md](LEARNING_GUIDE.md) and the [GKE tutorial](GKE_SETUP_TUTORIAL.html), which are served at `/docs/…`.

## What is measured

| Measurement | Start | End |
|---|---|---|
| `api_rtt` | KubeScope sends PATCH/DELETE | API server response received |
| `time_to_convergence` (scale) | PATCH acknowledged | first observation with `observedGeneration ≥ generation` and `updated = ready = available = status.replicas = desired` |
| `end_to_end` (scale) | PATCH sent | convergence observed |
| `time_to_replacement_observed` | DELETE acknowledged | first Pod whose UID did not exist before deletion |
| `time_to_replacement_ready` | DELETE acknowledged | that Pod's `Ready` condition is True |
| `time_to_desired_readiness` | DELETE acknowledged | Deployment converged at baseline with replacement Ready |
| `recovery_duration` | first observation of `readyReplicas` < baseline | desired readiness restored. **`null` if no dip was observed** |
| `time_to_original_absent` | DELETE acknowledged | original Pod UID gone from the API |

Readiness durations include scheduling, image pulls, container start, readiness-probe periods and watch/poll delivery. They are **not** control-plane latency. Statuses are `running`, `success`, `timeout`, `failed` and `interrupted` (cancelled, server shutdown, or `spec.replicas` changed by someone else).

## API (`/api/v1`)

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | process health |
| GET | `/connection` | context/cluster/server/namespace, reachability, watch status (`watchSynced`, `watchErrors`, `watchRestarts`), limits. `?refresh=true` pings now |
| GET | `/snapshot` | Deployment, ReplicaSets, Pods, Events, listed directly from the API server; `streamSeq` = SSE sequence at snapshot time |
| GET | `/events/stream` | SSE. Each message's `data` is `{seq,type,at,data}` and `id` is `seq`. Types: `hello`, `resource`, `k8s_event`, `experiment`, `connection`, `resync`. Supports `Last-Event-ID` replay from a 1024-event buffer; `resync` means "refetch /snapshot" |
| POST | `/experiments/scale` | `{"deployment":"demo-api","replicas":4}` → 202 + experiment |
| POST | `/experiments/delete-pod` | `{"podName":"…","confirm":true}` → 202 |
| POST | `/experiments/reset` | `{"confirm":true}` → scale to baseline (added) |
| POST | `/experiments/{id}/cancel` | interrupt a running experiment (added) |
| GET | `/experiments` | `{"experiments":[summary…]}`, newest first |
| GET | `/experiments/{id}` | full record incl. timeline |
| GET | `/experiments/{id}/export` | JSON download (`kubescope-experiments/v1`) |
| GET | `/experiments/export` | all history as one JSON download (added) |

Errors: `{"error":{"code":"…","message":"…"}}`. The codes from the spec are `INVALID_ARGUMENT` (400), `FORBIDDEN` (403), `CONFLICT` (409), `NOT_CONNECTED` (503), `TIMEOUT` (504) and `INTERNAL` (500). Two codes were added: `NOT_FOUND` (404) and `RATE_LIMITED` (429).

## Configuration

Set these as environment variables, or copy `.env.example` to `.env`, which `scripts/dev.sh` loads.

| Variable | Default | Meaning |
|---|---|---|
| `KUBESCOPE_CONTEXT` | kubeconfig current-context | kubeconfig context. If set and missing, KubeScope refuses to start the client; it never falls back to another context |
| `KUBECONFIG` | `~/.kube/config` | kubeconfig path(s) |
| `KUBESCOPE_NAMESPACE` | `kubescope-demo` | demo namespace |
| `KUBESCOPE_ALLOWED_NAMESPACES` | = namespace | allowlist; the namespace must be in it |
| `KUBESCOPE_DEPLOYMENT` | `demo-api` | the only Deployment that can be scaled |
| `KUBESCOPE_MIN_REPLICAS` / `MAX_REPLICAS` | 1 / 5 | max can be raised to 10 at most (hard limit) |
| `KUBESCOPE_BASELINE_REPLICAS` | 2 | target of *Reset* |
| `KUBESCOPE_EXPERIMENT_TIMEOUT` | `120s` | 5s–15m |
| `KUBESCOPE_MUTATIONS_PER_MINUTE` | 20 | rate limit on POST endpoints |
| `KUBESCOPE_ADDR` | `127.0.0.1:8080` | non-loopback addresses are refused… |
| `KUBESCOPE_ALLOW_NON_LOOPBACK` | `false` | …unless this is `true` (only behind your own auth) |
| `KUBESCOPE_ALLOWED_ORIGINS` | Vite dev origins | browser origins allowed to call POST endpoints |
| `KUBESCOPE_STATIC_DIR` | – | serve a built `frontend/dist` from the backend |
| `KUBESCOPE_IN_CLUSTER` | `false` | use the in-cluster ServiceAccount (not the supported MVP path) |

To run as a single process: `cd frontend && npm run build`, then `KUBESCOPE_STATIC_DIR=../frontend/dist go run ./cmd/server` from `backend/`, and open http://127.0.0.1:8080.

## Safety model

- The backend binds to localhost only. A Host-header check blocks DNS rebinding. POSTs require `Content-Type: application/json` and a same-origin or allowlisted `Origin`. Mutations are rate-limited, request bodies are capped at 4 KB, and unknown JSON fields are rejected.
- Every mutation is re-checked **in the backend** against the live object: the namespace must be on the allowlist and the object must carry `app.kubernetes.io/part-of=kubescope-demo`. A Pod must be controlled by a ReplicaSet owned by `demo-api` and must not already be terminating. Replicas must be within bounds. Pod deletion uses a UID precondition.
- There is one mutating experiment per Deployment at a time (HTTP 409 otherwise). Locks are released on success, failure, timeout and cancellation.
- No kubeconfig, token or credential is ever sent to the browser.
- Your personal kubeconfig is usually much more powerful than KubeScope needs. For defense in depth, `scripts/make-restricted-kubeconfig.sh <context>` applies `k8s/optional-rbac.yaml` (get/list/watch, plus patch on `deployments/demo-api` and delete pods, in one namespace) and writes `kubescope-restricted.kubeconfig` (8-hour token, gitignored) in the repo root. Run the backend with it from `backend/`: `KUBECONFIG=../kubescope-restricted.kubeconfig KUBESCOPE_CONTEXT=kubescope-restricted go run ./cmd/server`. On kind the smoke test passes with this identity, and `kubectl auth can-i delete pods -n default` is `no` for it. RBAC can't filter by label, so the backend's label check is still what limits Pod deletes to the demo app.

## Tests

```bash
./scripts/test.sh      # gofmt, go vet, go test -race ./..., frontend typecheck + build
```

- Unit tests cover convergence rules (including `observedGeneration` lag and spec-only false positives), safety and namespace/label enforcement, config validation, SSE sequence/replay/resync, informer restart after an outage, timeline offsets, and the experiment state machine: success, external change → interrupted, timeout, cancel, conflict, failure unlocking, Pod-UID replacement, and the no-dip case.
- API tests run the real HTTP server, watcher and engine against client-go's **fake clientset driven by a tiny test-only simulator** (`backend/internal/testsim`). This validates KubeScope's logic and API contract. It says **nothing** about real cluster timing.
- `scripts/smoke-test.sh` is the real-cluster integration test. It has passed on kind; see [IMPLEMENTATION_NOTES.md](IMPLEMENTATION_NOTES.md) for the measured values.

## Repository layout

```
backend/   cmd/server · internal/{config,kube,experiments,httpapi,safety,models,stream,testsim}
frontend/  src/{App.tsx,api/,components/,styles/} · tsconfig.{app,node}.json
k8s/       demo-namespace.yaml · demo-deployment.yaml · optional-rbac.yaml · kind-config.yaml
scripts/   kind-up.sh · kind-down.sh · dev.sh · smoke-test.sh · test.sh · make-restricted-kubeconfig.sh
docs/      screenshots/ (real kind screenshots) · results/ (exported smoke-test JSON)
```

Compared with the spec layout, this adds `internal/stream` (SSE hub), `internal/testsim` (test-only simulator), `k8s/kind-config.yaml`, `docs/` and the extra scripts.

## Cleanup

```bash
# stop the backend and the dashboard: Ctrl+C in their terminals (or Ctrl+C on scripts/dev.sh)
./scripts/kind-down.sh     # delete the kind cluster
colima stop                # macOS: stop the Docker VM
# GKE: see "Clean up immediately" in GKE_SETUP_TUTORIAL.html. Stopping KubeScope does not stop GKE billing.
```
