import type { ApiError } from '../api/client';
import type { Snapshot } from '../api/types';
import { ago, shortUid } from './format';

interface Props {
  snapshot: Snapshot | null;
  error: ApiError | null;
  loading: boolean;
  canMutate: boolean;
  onDeletePod: (name: string, uid: string) => void;
}

export function DeploymentPanel({ snapshot, error, loading, canMutate, onDeletePod }: Props) {
  if (error) {
    return (
      <section className="panel" aria-labelledby="dep-h">
        <h2 id="dep-h">Demo Deployment</h2>
        <div className="empty error-box" role="alert">
          <strong>{error.code === 'NETWORK' ? 'Cannot reach the KubeScope backend.' : error.code === 'NOT_CONNECTED' ? 'Cannot reach the cluster.' : 'Could not load resources.'}</strong>
          <p>{error.message}</p>
          <p className="muted">No cluster data is shown because none could be fetched — this is not an empty cluster.</p>
        </div>
      </section>
    );
  }
  if (!snapshot) {
    return (
      <section className="panel" aria-labelledby="dep-h" aria-busy={loading}>
        <h2 id="dep-h">Demo Deployment</h2>
        <div className="empty">{loading ? 'Loading snapshot…' : 'No snapshot yet.'}</div>
      </section>
    );
  }
  const d = snapshot.deployment;
  return (
    <section className="panel" aria-labelledby="dep-h">
      <div className="panel-head">
        <h2 id="dep-h">Demo Deployment {d && <code>{d.name}</code>}</h2>
        <span className="muted small">snapshot {ago(snapshot.observedAt)}</span>
      </div>
      {snapshot.warnings?.map((w) => (
        <p key={w} className="warning">{w}</p>
      ))}
      {d ? (
        <>
          <div className="counts" role="group" aria-label="Replica counts">
            <Count label="Desired" value={d.desired} hint="spec.replicas — what you asked for" />
            <Count label="Current" value={d.current} hint="status.replicas — non-terminated Pods" />
            <Count label="Updated" value={d.updated} hint="Pods on the latest template" />
            <Count label="Ready" value={d.ready} hint="Pods whose readiness probe passes" />
            <Count label="Available" value={d.available} hint="Ready for at least minReadySeconds" />
          </div>
          <p className={`converge ${d.converged ? 'ok' : 'warn'}`}>
            {d.converged ? '✓ Converged' : '… Reconciling'} — <span className="muted">{d.convergenceReason}</span>
          </p>
          <p className="muted small">
            generation {d.generation} · observedGeneration {d.observedGeneration} · image <code>{d.image}</code>
          </p>
        </>
      ) : (
        <div className="empty">Deployment not found in this namespace.</div>
      )}

      <h3>Pods ({snapshot.pods.length})</h3>
      {snapshot.pods.length === 0 ? (
        <div className="empty">No demo Pods.</div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">UID</th>
                <th scope="col">Ready</th>
                <th scope="col">Running</th>
                <th scope="col">Phase</th>
                <th scope="col" className="col-node">Node</th>
                <th scope="col" className="col-action"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {snapshot.pods.map((p) => (
                <tr key={p.uid} className={p.terminating ? 'terminating' : ''}>
                  <td><code title={p.name}>{shortPod(p.name, d?.name)}</code></td>
                  <td><code title={p.uid}>{shortUid(p.uid)}</code></td>
                  <td><span className={`badge ${p.ready ? 'ok' : 'warn'}`}>{p.ready ? 'Ready' : 'Not ready'}</span></td>
                  <td>{p.running ? 'yes' : 'no'}</td>
                  <td>{p.terminating ? 'Terminating' : p.phase}{p.reason ? ` (${p.reason})` : ''}</td>
                  <td className="col-node" title={p.nodeName || undefined}>{p.nodeName || <span className="muted">unscheduled</span>}</td>
                  <td className="col-action">
                    <button className="btn danger small" disabled={!canMutate || p.terminating} onClick={() => onDeletePod(p.name, p.uid)}>
                      Delete…
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="muted small">
        <strong>Running</strong> means the container process started. <strong>Ready</strong> means its readiness probe passes and it would receive Service traffic.
      </p>
    </section>
  );
}

function Count({ label, value, hint }: { label: string; value: number; hint: string }) {
  return (
    <div className="count" title={hint}>
      <span className="count-value">{value}</span>
      <span className="count-label">{label}</span>
    </div>
  );
}

// demo-api-7d9f8c6b5-x2k4q → …-x2k4q (full name in the tooltip and confirm dialog)
function shortPod(name: string, dep?: string): string {
  if (dep && name.startsWith(dep + '-')) {
    return '…-' + name.split('-').pop();
  }
  return name;
}
