import type { ApiError } from '../api/client';
import type { Connection } from '../api/types';
import type { StreamState } from '../api/useLive';
import { ago } from './format';

interface Props {
  connection: Connection | null;
  connectionError: ApiError | null;
  stream: StreamState;
  lastRefresh: Date | null;
  onRefresh: () => void;
}

export function ConnectionPanel({ connection: c, connectionError, stream, lastRefresh, onRefresh }: Props) {
  let state: 'ok' | 'warn' | 'bad' = 'bad';
  let label = 'Backend unreachable';
  if (connectionError) {
    // The KubeScope backend itself did not answer. Any details below are the
    // last known values, so never show them under a green "Connected" pill.
    label = c ? 'Backend unreachable (showing last known details)' : 'Backend unreachable';
  } else if (c) {
    if (!c.configured) label = 'Not connected (kubeconfig/context problem)';
    else if (!c.reachable) label = 'API server unreachable';
    else {
      state = c.watchSynced ? 'ok' : 'warn';
      label = c.watchSynced ? 'Connected' : 'Connected (watch syncing)';
    }
  }
  return (
    <section className="panel connection" aria-labelledby="conn-h">
      <div className="panel-head">
        <h2 id="conn-h">Connection</h2>
        <button className="btn ghost" onClick={onRefresh}>Refresh</button>
      </div>
      <p className={`status-pill ${state}`} role="status">
        <span className="dot" aria-hidden="true" /> {label}
      </p>
      {connectionError && <p className="error-text">{connectionError.message}</p>}
      {c && (
        <dl className="kv">
          <dt>Context</dt>
          <dd><code>{c.context || '—'}</code></dd>
          <dt>Cluster</dt>
          <dd><code>{c.cluster || '—'}</code></dd>
          <dt>API server</dt>
          <dd><code>{c.server || '—'}</code>{c.serverVersion ? ` (${c.serverVersion})` : ''}</dd>
          <dt>Namespace</dt>
          <dd><code>{c.namespace}</code></dd>
          <dt>Last successful refresh</dt>
          <dd>{c.lastSuccessAt ? ago(c.lastSuccessAt) : 'never'}{lastRefresh ? ` · snapshot ${ago(lastRefresh)}` : ''}</dd>
          <dt>Live stream</dt>
          <dd>{stream}{c.watchErrors > 0 ? ` · ${c.watchErrors} watch error(s), auto-retrying` : ''}{c.watchRestarts > 0 ? ` · watches restarted ${c.watchRestarts}× after API outages` : ''}</dd>
        </dl>
      )}
      {c?.error && <p className="error-text" title={c.error}>Last error: {c.error}</p>}
      {c?.isGKE && (
        <p className="warning" role="alert">
          <strong>GKE context selected.</strong> Experiments change a real cloud cluster you pay for. Double-check the
          context and project before running anything, and delete the cluster when you are done.
        </p>
      )}
    </section>
  );
}
