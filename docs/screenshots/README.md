# Screenshots — real kind cluster

Captured on 2026-11-16 from the running dashboard (`npm run dev`, http://127.0.0.1:5173) in the Claude desktop
app's built-in browser. The backend was connected to a **real local kind cluster** (context `kind-kubescope`,
Kubernetes v1.37.0, kind v0.33.0, Colima on an Apple Silicon Mac). The JPEG captures were converted to PNG with
`sips`; nothing else was edited.

| File | What it shows |
|---|---|
| `kind-scale-2-to-4.png` | Scale 2 → 4 started from the UI: `success`, API RTT 5 ms, acknowledged → converged 3.24 s, start of the timeline |
| `kind-delete-pod-confirm.png` | The confirmation dialog before deleting a demo Pod (name, UID, namespace, context) |
| `kind-pod-recovery.png` | Pod deletion from the UI: replacement Pod with a new UID, all six recovery measurements |
| `kind-pod-recovery-timeline.png` | The same experiment's timeline (Observed vs Explanation entries) and the Pod table |
| `kind-backend-unreachable.png` | The Go backend stopped while the page was open: red “Backend unreachable” state, controls disabled |
| `kind-api-server-unreachable.png` | `docker stop kubescope-control-plane`: “API server unreachable” with the real connection error |
| `kind-mobile.png` | The dashboard at a 375 px wide mobile viewport |

Notes:

- `kind-scale-2-to-4.png` and `kind-delete-pod-confirm.png` were taken before a timeline-offset fix
  in the backend, so entries recorded before the request may show a small positive offset there.
  `kind-pod-recovery*.png` were retaken after the fix.
- The numbers are single observations on one laptop, not benchmarks.
- The earlier `simulated-*.png` images (from a fake test cluster) were deleted.
