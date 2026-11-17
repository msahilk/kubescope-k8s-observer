#!/usr/bin/env python3
"""Repeated KubeScope experiments with a per-stage breakdown.

Runs N rounds of (scale 2->4, scale 4->2, delete one Pod) through a RUNNING
KubeScope backend, waits for the demo Deployment to settle between
experiments, saves every experiment record, and summarises the durations.

    ./scripts/benchmark.py run --api http://127.0.0.1:8080 --label kind --runs 10
    ./scripts/benchmark.py run --api http://127.0.0.1:8080 --label gke --runs 10 --allow-gke
    ./scripts/benchmark.py compare docs/benchmarks/kind.json docs/benchmarks/gke.json

Stage durations are differences between two observations that reached
KubeScope over the same watch connection, so the one-way network delay
roughly cancels out. They still include KubeScope's own observation delay
and watch-delivery jitter. Standard library only.
"""
import argparse
import json
import re
import statistics
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

BASELINE = 2
SCALE_TO = 4
MIN_POST_SPACING_S = 3.5  # stays under KubeScope's default 20 mutations/minute


# ---------------------------------------------------------------- HTTP helpers

class Api:
    def __init__(self, base):
        self.base = base.rstrip("/") + "/api/v1"
        self.last_post = 0.0

    def get(self, path):
        with urllib.request.urlopen(self.base + path, timeout=30) as r:
            return json.load(r)

    def post(self, path, body):
        for attempt in range(20):
            wait = self.last_post + MIN_POST_SPACING_S - time.monotonic()
            if wait > 0:
                time.sleep(wait)
            req = urllib.request.Request(self.base + path, data=json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json"}, method="POST")
            self.last_post = time.monotonic()
            try:
                with urllib.request.urlopen(req, timeout=60) as r:
                    return json.load(r)
            except urllib.error.HTTPError as e:
                if e.code in (409, 429):  # experiment still running / rate limited
                    time.sleep(5)
                    continue
                raise SystemExit(f"POST {path} failed: HTTP {e.code} {e.read().decode()[:300]}")
        raise SystemExit(f"POST {path}: gave up after repeated 409/429")


def wait_experiment(api, exp_id, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        x = api.get(f"/experiments/{exp_id}")
        if x["status"] != "running":
            return x
        time.sleep(0.5)
    raise SystemExit(f"experiment {exp_id} still running after {timeout}s")


def wait_settled(api, desired, timeout=180):
    """Deployment converged at `desired`, exactly `desired` Pods, none terminating."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        s = api.get("/snapshot")
        d = s.get("deployment") or {}
        pods = s.get("pods", [])
        if (d.get("converged") and d.get("desired") == desired and len(pods) == desired
                and all(p["ready"] and not p["terminating"] for p in pods)):
            return
        time.sleep(1)
    raise SystemExit(f"cluster did not settle at {desired} replicas within {timeout}s")


# ---------------------------------------------------------------- analysis

def ts(s):
    """Parse Go RFC3339Nano (any number of fractional digits) to an aware datetime."""
    m = re.match(r"(.*T\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", s)
    frac = (m.group(2) or "0")[:6].ljust(6, "0")
    tz = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.fromisoformat(f"{m.group(1)}.{frac}{tz}")


def ms(a, b):
    return None if a is None or b is None else round((b - a).total_seconds() * 1000, 1)


def pod_transitions(exp, baseline_names):
    """First observation of each state for Pods created during the experiment."""
    pods = {}
    for e in exp["timeline"]:
        res = e.get("resource") or ""
        if e["kind"] != "watch" or not res.startswith("Pod/"):
            continue
        name = res[4:]
        if name in baseline_names:
            continue
        msg, at = e["message"], ts(e["at"])
        p = pods.setdefault(name, {})
        if msg.startswith("ADDED"):
            p.setdefault("added", at)
        if "node=" in msg:
            p.setdefault("scheduled", at)
        if "phase=Running" in msg:
            p.setdefault("running", at)
        if "ready=true" in msg:
            p.setdefault("ready", at)
    return pods


def first_entry(exp, pred):
    for e in exp["timeline"]:
        if pred(e):
            return ts(e["at"])
    return None


def measurement(exp, name):
    for m in exp["measurements"]:
        if m["name"] == name:
            return m["valueMs"]
    return None


def analyse(exp, baseline_names):
    """Return {metric: value_ms} for one experiment."""
    out = {"api_rtt": measurement(exp, "api_rtt")}
    ack = ts(exp["requestAcknowledgedAt"]) if exp.get("requestAcknowledgedAt") else None
    conv = ts(exp["convergedAt"]) if exp.get("convergedAt") else None
    pods = pod_transitions(exp, baseline_names)
    new = [p for p in pods.values() if "added" in p]
    if exp["type"] == "scale":
        up = exp["target"]["toReplicas"] > exp["target"]["fromReplicas"]
        out["time_to_convergence"] = measurement(exp, "time_to_convergence")
        rs = first_entry(exp, lambda e: e["kind"] == "watch" and (e.get("resource") or "").startswith("ReplicaSet/")
                         and f"spec.replicas={exp['target']['toReplicas']}" in e["message"])
        out["stage_controller_resize_rs"] = ms(ack, rs)
        if up and new:
            first_added = min(p["added"] for p in new)
            out["stage_rs_to_pod_created"] = ms(rs, first_added)
            last_ready = max((p["ready"] for p in new if "ready" in p), default=None)
            out["stage_last_pod_ready_to_converged"] = ms(last_ready, conv)
    else:
        out["time_to_replacement_observed"] = measurement(exp, "time_to_replacement_observed")
        out["time_to_desired_readiness"] = measurement(exp, "time_to_desired_readiness")
        out["time_to_original_absent"] = measurement(exp, "time_to_original_absent")
    per_pod = {"stage_pod_scheduling": [], "stage_kubelet_start": [], "stage_readiness_probe": [],
               "stage_pod_created_to_ready": []}
    for p in new:
        per_pod["stage_pod_scheduling"].append(ms(p.get("added"), p.get("scheduled")))
        per_pod["stage_kubelet_start"].append(ms(p.get("scheduled"), p.get("running")))
        per_pod["stage_readiness_probe"].append(ms(p.get("running"), p.get("ready")))
        per_pod["stage_pod_created_to_ready"].append(ms(p.get("added"), p.get("ready")))
    return out, {k: [v for v in vs if v is not None] for k, vs in per_pod.items()}


METRICS = [
    # (group, key, label, explanation)
    ("Network", "api_rtt", "API request round-trip", "PATCH/DELETE sent → response received (all experiment types)"),
    ("Scale 2→4", "scale_up.time_to_convergence", "Acknowledged → converged", "PATCH acknowledged → Deployment converged at 4"),
    ("Scale 2→4", "scale_up.stage_controller_resize_rs", "Deployment controller", "PATCH acknowledged → ReplicaSet spec=4 observed"),
    ("Scale 2→4", "scale_up.stage_rs_to_pod_created", "ReplicaSet controller", "ReplicaSet spec=4 observed → first new Pod observed"),
    ("Scale 2→4", "scale_up.stage_last_pod_ready_to_converged", "Status roll-up", "last new Pod Ready → Deployment converged observed"),
    ("Scale 4→2", "scale_down.time_to_convergence", "Acknowledged → converged", "PATCH acknowledged → Deployment reports 2 (Pods may still be terminating)"),
    ("Scale 4→2", "scale_down.stage_controller_resize_rs", "Deployment controller", "PATCH acknowledged → ReplicaSet spec=2 observed"),
    ("Pod deletion", "delete.time_to_replacement_observed", "Replacement Pod created", "DELETE acknowledged → Pod with a new UID observed"),
    ("Pod deletion", "delete.time_to_desired_readiness", "Back to full readiness", "DELETE acknowledged → Deployment converged with replacement Ready"),
    ("Pod deletion", "delete.time_to_original_absent", "Original Pod gone", "DELETE acknowledged → original Pod removed (graceful shutdown)"),
    ("Per new Pod", "pods.stage_pod_scheduling", "Scheduler", "Pod created → node assigned"),
    ("Per new Pod", "pods.stage_kubelet_start", "Kubelet start", "node assigned → phase Running (sandbox + container start)"),
    ("Per new Pod", "pods.stage_readiness_probe", "Readiness probe", "Running → Ready (probe: initialDelay 2 s, period 2 s)"),
    ("Per new Pod", "pods.stage_pod_created_to_ready", "Pod created → Ready", "whole per-Pod path"),
]


def summarise(experiments, baselines):
    buckets = {}

    def add(key, v):
        if v is not None:
            buckets.setdefault(key, []).append(v)

    for exp in experiments:
        out, per_pod = analyse(exp, set(baselines.get(exp["id"], [])))
        if exp["type"] == "scale":
            prefix = "scale_up" if exp["target"]["toReplicas"] > exp["target"]["fromReplicas"] else "scale_down"
        else:
            prefix = "delete"
        add("api_rtt", out.pop("api_rtt"))
        for k, v in out.items():
            add(f"{prefix}.{k}", v)
        for k, vs in per_pod.items():
            for v in vs:
                add(f"pods.{k}", v)
    stats = {}
    for k, vs in buckets.items():
        stats[k] = {"n": len(vs), "median": round(statistics.median(vs), 1), "min": min(vs), "max": max(vs)}
    return stats


def fmt(s):
    if not s:
        return "—"
    if s["median"] >= 1000:
        return f"{s['median'] / 1000:.2f} s ({s['min'] / 1000:.2f}–{s['max'] / 1000:.2f})"
    return f"{s['median']:.0f} ms ({s['min']:.0f}–{s['max']:.0f})"


# ---------------------------------------------------------------- commands

def cmd_run(a):
    api = Api(a.api)
    conn = api.get("/connection?refresh=true")
    if not conn.get("reachable"):
        raise SystemExit(f"cluster not reachable: {conn.get('error')}")
    if conn.get("isGKE") and not a.allow_gke:
        raise SystemExit("refusing to run against a GKE context without --allow-gke")
    print(f"context={conn['context']} server={conn.get('serverVersion')} gke={conn.get('isGKE')} runs={a.runs}", flush=True)

    snap = api.get("/snapshot")
    if (snap.get("deployment") or {}).get("desired") != BASELINE:
        print(f"resetting to baseline {BASELINE}…", flush=True)
        wait_experiment(api, api.post("/experiments/reset", {"confirm": True})["id"])
    wait_settled(api, BASELINE)

    experiments, baselines = [], {}

    def record(exp_id, settle_at):
        x = wait_experiment(api, exp_id)
        experiments.append(x)
        print(f"  {x['type']:<10} {x['id']} {x['status']}", flush=True)
        if x["status"] != "success":
            raise SystemExit(f"experiment {x['id']} ended with {x['status']}: {x.get('statusReason')}")
        wait_settled(api, settle_at)

    started = datetime.now(timezone.utc)
    for i in range(1, a.runs + 1):
        print(f"round {i}/{a.runs}", flush=True)
        for target in (SCALE_TO, BASELINE):
            names = [p["name"] for p in api.get("/snapshot")["pods"]]
            x = api.post("/experiments/scale", {"deployment": "demo-api", "replicas": target})
            baselines[x["id"]] = names
            record(x["id"], target)
        snap = api.get("/snapshot")
        names = [p["name"] for p in snap["pods"]]
        pod = next(p["name"] for p in snap["pods"] if p["ready"] and not p["terminating"])
        x = api.post("/experiments/delete-pod", {"podName": pod, "confirm": True})
        baselines[x["id"]] = names
        record(x["id"], BASELINE)

    result = {
        "format": "kubescope-benchmark/v1",
        "label": a.label,
        "startedAt": started.isoformat(),
        "finishedAt": datetime.now(timezone.utc).isoformat(),
        "context": conn["context"],
        "cluster": conn.get("cluster"),
        "serverVersion": conn.get("serverVersion"),
        "isGKE": conn.get("isGKE"),
        "rounds": a.runs,
        "summary": summarise(experiments, baselines),
        "baselinePods": baselines,
        "experiments": experiments,
        "notes": [
            "All timestamps are taken by KubeScope on the client machine.",
            "Stage durations are differences between two watch observations, so the one-way network delay roughly cancels; they still include watch-delivery jitter.",
            "Summary values are milliseconds: n samples, median, min, max. No percentiles at this sample size.",
        ],
    }
    with open(a.out, "w") as f:
        json.dump(result, f, indent=1, default=str)
    print(f"wrote {a.out}")
    for group, key, label, _ in METRICS:
        print(f"  {group:<13} {label:<26} {fmt(result['summary'].get(key))}  n={result['summary'].get(key, {}).get('n', 0)}")


def cmd_compare(a):
    runs = [json.load(open(p)) for p in a.files]
    head = "| Group | Stage | What it measures | " + " | ".join(f"{r['label']} (median, min–max)" for r in runs) + " |"
    print(head)
    print("|" + "---|" * (3 + len(runs)))
    for group, key, label, expl in METRICS:
        cells = []
        for r in runs:
            s = r["summary"].get(key)
            cells.append(f"{fmt(s)}, n={s['n']}" if s else "—")
        print(f"| {group} | {label} | {expl} | " + " | ".join(cells) + " |")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--api", default="http://127.0.0.1:8080")
    r.add_argument("--label", required=True)
    r.add_argument("--runs", type=int, default=10)
    r.add_argument("--out")
    r.add_argument("--allow-gke", action="store_true")
    c = sub.add_parser("compare")
    c.add_argument("files", nargs="+")
    a = ap.parse_args()
    if a.cmd == "run":
        if not 1 <= a.runs <= 30:
            raise SystemExit("--runs must be between 1 and 30")
        a.out = a.out or f"benchmark-{a.label}.json"
        cmd_run(a)
    else:
        cmd_compare(a)


if __name__ == "__main__":
    sys.exit(main())
