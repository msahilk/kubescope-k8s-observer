# kind vs GKE: what changes when the cluster moves to Google Cloud

_Measured 2026-11-17 with `scripts/benchmark.py`, 10 rounds per cluster. Raw data: [`benchmarks/kind.json`](benchmarks/kind.json) and [`benchmarks/gke.json`](benchmarks/gke.json)._

## The short answer

- **The same things happen in the same order on both.** The API server stores the change, the Deployment controller resizes the ReplicaSet, the ReplicaSet controller creates Pods, the scheduler places them, the kubelet starts the containers, and the readiness probe passes. All 60 experiments (30 per cluster) succeeded, and KubeScope needed no code changes.
- **The network explains only part of the difference.** An API request took about 72 ms on GKE versus 8 ms on kind, mostly because each request crosses the internet (~30 ms per round-trip to us-central1).
- **Google's side was not faster.** With the network delay removed, every control-plane step still took longer on GKE: tens of milliseconds per step instead of single digits. Starting a container on the node took about 1 s instead of about 0.2 s.
- **Neither of those dominates what you wait for.** On both clusters, most of the time before a new Pod is Ready is the readiness probe (`initialDelaySeconds: 2`, `periodSeconds: 2`). That's a setting in our own manifest, not cluster speed.

Overall, scaling 2→4 took a median **2.77 s on kind and 4.11 s on GKE**, and recovering from a Pod deletion took **2.73 s vs 3.60 s**.

## Setup

| | kind | GKE |
|---|---|---|
| Where | Laptop: Colima VM (2 CPU, 4 GB) on an Apple Silicon Mac | Google Cloud `us-central1-a`, zonal GKE Standard |
| Kubernetes | v1.37.0 | v1.36.4-gke.2377000 |
| Nodes | 1 container (`kubescope-control-plane`, arm64) running **both** the control plane and the Pods | 1 VM `e2-standard-2` (2 vCPU, 8 GB, amd64, Container-Optimized OS). The control plane is Google-managed and separate |
| Network from KubeScope to the API server | Same machine (port forward) | Internet; TCP connect ≈ 30 ms |
| Demo workload | `k8s/demo-deployment.yaml`, unchanged | same |
| Image | Already on the node for every new Pod ("already present on machine", 30/30) | same, 30/30 |
| KubeScope | Backend on the laptop, `KUBESCOPE_CONTEXT=kind-kubescope` | Backend on the laptop, `KUBESCOPE_CONTEXT=gke_kubescope-lab_us-central1-a_kubescope-lab` |

The two benchmarks ran one after the other (GKE 03:23–03:26 UTC, kind 03:26–03:28 UTC), not at the same time, so they didn't compete for the laptop.

## Method

Each round runs **scale 2→4**, **scale 4→2** and **delete one Pod** through KubeScope's API. Before the next experiment starts, the script waits until the Deployment has settled (converged, no terminating Pods), so experiments don't overlap. Every number is taken by KubeScope on the laptop.

The interesting part is the **stage breakdown**. KubeScope records the moment it *receives* each watch event: Pod created, Pod assigned to a node, Pod Running, Pod Ready. Watch events arrive over the same connection, so each one is delayed by roughly the same one-way network time. Subtracting two of them leaves mostly the time the cluster spent between those two states. That's how we can talk about Google's control plane separately from the internet. It isn't perfect: watch delivery has jitter, and the kubelet reports Pod status asynchronously, which blurs the line between "Kubelet start" and "Readiness probe". Their sum, "Pod created → Ready", is the more robust number.

Values are **median (min–max)**. With n = 10 or 30 we don't report percentiles.

## Results

| Group | Stage | What it measures | kind | GKE |
|---|---|---|---|---|
| Network | API request round-trip | PATCH/DELETE sent → response received | 8 ms (2–20), n=30 | 72 ms (54–133), n=30 |
| Scale 2→4 | **Acknowledged → converged** | PATCH acknowledged → Deployment converged at 4 | **2.77 s** (2.46–3.14), n=10 | **4.11 s** (3.38–5.12), n=10 |
| Scale 2→4 | Deployment controller | PATCH acknowledged → ReplicaSet `spec.replicas=4` observed | 7 ms (4–10) | 48 ms (43–1216) |
| Scale 2→4 | ReplicaSet controller | ReplicaSet resized → first new Pod observed | 9 ms (3–14) | 67 ms (45–97) |
| Scale 2→4 | Status roll-up | last new Pod Ready → Deployment converged | 16 ms (10–53) | 95 ms (26–166) |
| Scale 4→2 | Acknowledged → converged | PATCH acknowledged → Deployment reports 2 (old Pods may still be terminating) | 35 ms (15–66) | 208 ms (174–425) |
| Scale 4→2 | Deployment controller | PATCH acknowledged → ReplicaSet `spec.replicas=2` observed | 6 ms (2–17) | 45 ms (36–87) |
| Pod deletion | Replacement Pod created | DELETE acknowledged → Pod with a new UID observed | 16 ms (10–21) | 66 ms (61–123) |
| Pod deletion | **Back to full readiness** | DELETE acknowledged → Deployment converged with the replacement Ready | **2.73 s** (2.45–3.06) | **3.60 s** (2.63–4.73) |
| Pod deletion | Original Pod gone | DELETE acknowledged → original Pod removed (graceful shutdown) | 706 ms (425–1037) | 1.37 s (0.61–2.12) |
| Per new Pod | Scheduler | Pod created → node assigned | 7 ms (3–16), n=30 | 44 ms (32–443), n=30 |
| Per new Pod | Kubelet start | node assigned → phase Running | 174 ms (136–280) | 1.02 s (0.70–1.75) |
| Per new Pod | Readiness probe | Running → Ready | 2.53 s (2.22–2.88) | 2.23 s (1.10–2.88) |
| Per new Pod | **Pod created → Ready** | the whole per-Pod path | **2.69 s** (2.41–3.10) | **3.25 s** (2.19–4.58) |

Regenerate this table with `./scripts/benchmark.py compare docs/benchmarks/kind.json docs/benchmarks/gke.json`.

### Where the time goes in a 2→4 scale-up (medians, approximate)

```
kind  |▏controllers ~16 ms|▏kubelet ~0.2 s|██████████ readiness probe ~2.5 s|▏roll-up|          ≈ 2.8 s
GKE   |▌controllers ~115 ms|████ kubelet ~1.0 s|█████████ readiness probe ~2.2 s|▌roll-up ~0.1 s| ≈ 4.1 s
```

The stage medians don't add up exactly to the total. A scale-up finishes when the *slower* of its two new Pods is Ready, so the total is closer to the per-Pod maximum than to the per-Pod median.

## What's the same

- **The reconcile chain.** Both timelines show the same sequence of watch events and Kubernetes Events (`ScalingReplicaSet` → `SuccessfulCreate` → `Scheduled` → `Pulled` → `Created` → `Started`), and replacement Pods always get a new UID.
- **The readiness probe dominates.** On both clusters about 2–2.5 s of a new Pod's ~3 s life before Ready is spent waiting for the probe. The probe first runs 2 s after the container starts and then every 2 s. Lowering `initialDelaySeconds` would speed up both clusters more than anything Google or your laptop does.
- **Scale-down "converges" before the old Pods are gone.** On both, the Deployment stops counting Pods as soon as they're marked for deletion (35 ms on kind, 208 ms on GKE), while those Pods are still shutting down.
- **Safety and measurement logic.** Same validation, same convergence rules, same results.

## What's different, and the likely reasons

1. **API round-trip: 8 ms vs 72 ms.** About 30 ms of that is one internet round-trip. The other ~40 ms is time on Google's side: authentication, admission checks, audit logging and a durable, replicated database write. On kind, the API server, etcd and KubeScope share one machine.
2. **Each control-plane hop: ~5–15 ms on kind vs ~45–95 ms on GKE**, with the network delay removed. Each hop (controller reads a change → writes a change → the write is stored → watchers are told) involves at least one database write. kind has a single etcd on the same VM, with no replication and no network between components. GKE's control plane is built for durability and availability, which costs latency per write. We can't see inside it, so this is an inference, not a measurement.
3. **Kubelet start: 0.17 s vs 1.02 s.** On GKE the node is a separate VM that talks to the control plane over Google's network and sets up GKE's Pod networking for each new Pod. The kubelet's status reports also reach the API server later. Part of this "start" time is probably the delay before the kubelet *reports* Running, which is also why GKE's "readiness probe" stage looks shorter. The sum (Pod created → Ready: 2.69 s vs 3.25 s) is the fair comparison.
4. **Graceful shutdown: 0.7 s vs 1.4 s.** Same 5 s grace period on both; the Pod object is removed after the kubelet confirms the container stopped, and that confirmation has further to travel on GKE.
5. **More spread on GKE.** Occasional outliers, for example one controller hop of 1.2 s and one scheduling of 443 ms, against kind's tight ranges. That's normal for a shared, networked system.

## So is Google's control plane faster?

**Not by these measurements.** Each step took longer on GKE even after taking the internet out of the comparison. Measuring "fast" this way favours kind, though: kind is one VM with everything in memory next to everything else, and nothing is replicated or durable beyond that laptop. GKE trades a few tens of milliseconds per step for a control plane that Google runs, upgrades, backs up and keeps available, plus real nodes that can be added, replaced and spread across machines. What GKE is better at is scale and reliability: many nodes, node failures, upgrades without you running etcd. A 1-node, 4-Pod demo doesn't exercise any of that.

## Limits of this comparison

- **One cluster of each kind, one time window, 10 rounds.** Different hardware (arm64 laptop VM vs amd64 cloud VM) and different Kubernetes versions (1.37 vs 1.36).
- **All timings are from the laptop.** They include KubeScope's own watch delivery, and on GKE the stage values still include network *jitter*.
- **Not tested:** a multi-node GKE cluster, Autopilot, a regional cluster, a different GKE region, or a cold image pull (the image was already on the node for every new Pod).
- **GKE internals** (storage, admission chain) are inferred from public descriptions, not observed.

## Reproduce

```bash
# kind (backend on :8080)
./scripts/benchmark.py run --api http://127.0.0.1:8080 --label kind --runs 10 --out docs/benchmarks/kind.json

# GKE (backend started with the GKE context; costs money while the cluster exists)
./scripts/benchmark.py run --api http://127.0.0.1:8080 --label gke --runs 10 --allow-gke --out docs/benchmarks/gke.json

./scripts/benchmark.py compare docs/benchmarks/kind.json docs/benchmarks/gke.json
```

The GKE cluster used here (1 × `e2-standard-2`, zonal, about $0.08/hour) was deleted right after the run; a check afterwards found no clusters, VMs, disks, external IPs or load balancers left in the project.
