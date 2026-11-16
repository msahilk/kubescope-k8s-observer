import { useCallback, useState } from 'react';
import { api, ApiError } from './api/client';
import type { Experiment } from './api/types';
import { useLive } from './api/useLive';
import { ConfirmDialog } from './components/ConfirmDialog';
import { ConnectionPanel } from './components/ConnectionPanel';
import { DeploymentPanel } from './components/DeploymentPanel';
import { ExperimentControls } from './components/ExperimentControls';
import { ExperimentView } from './components/ExperimentView';
import { HistoryPanel } from './components/HistoryPanel';
import { LearnPanel } from './components/LearnPanel';
import { LiveFeed } from './components/LiveFeed';

type Pending = { kind: 'delete'; name: string; uid: string } | { kind: 'reset' } | null;

export default function App() {
  const live = useLive();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending>(null);

  const c = live.connection;
  const canMutate = !!(c?.configured && c.reachable && live.snapshot?.deployment);

  const run = useCallback(
    async (fn: () => Promise<Experiment>) => {
      setBusy(true);
      setActionError(null);
      try {
        live.setCurrent(await fn());
        void live.reloadHistory();
      } catch (e) {
        const err = e as ApiError;
        setActionError(`${err.code}: ${err.message}`);
        void live.reloadHistory();
      } finally {
        setBusy(false);
      }
    },
    [live],
  );

  const deployment = c?.deployment ?? 'demo-api';

  return (
    <div className="app">
      <header className="app-header">
        <div>
          <h1>KubeScope</h1>
          <p className="tagline">Watch the Kubernetes control plane reconcile desired state — on kind or GKE.</p>
        </div>
        {c?.isGKE && <span className="badge warn big">GKE</span>}
      </header>

      {actionError && (
        <div className="toast error-box" role="alert">
          <span>{actionError}</span>
          <button className="btn ghost small" onClick={() => setActionError(null)} aria-label="Dismiss error">✕</button>
        </div>
      )}

      <main className="grid">
        <div className="col">
          <ConnectionPanel
            connection={c}
            connectionError={live.connectionError}
            stream={live.stream}
            lastRefresh={live.lastRefresh}
            onRefresh={() => void live.refresh()}
          />
          <ExperimentControls
            connection={c}
            snapshot={live.snapshot}
            current={live.current}
            busy={busy}
            canMutate={canMutate}
            onScale={(n) => void run(() => api.scale(deployment, n))}
            onReset={() => setPending({ kind: 'reset' })}
            onCancel={(id) => void run(() => api.cancel(id))}
          />
          <DeploymentPanel
            snapshot={live.snapshot}
            error={live.snapshotError}
            loading={live.snapshotLoading}
            canMutate={canMutate && live.current?.status !== 'running' && !busy}
            onDeletePod={(name, uid) => setPending({ kind: 'delete', name, uid })}
          />
        </div>
        <div className="col wide">
          <ExperimentView exp={live.current} />
          <LiveFeed feed={live.feed} events={live.snapshot?.events ?? []} />
        </div>
      </main>
      <HistoryPanel
        history={live.history}
        onOpen={(id) =>
          void api.experiment(id).then(live.setCurrent, (e: ApiError) => setActionError(`${e.code}: ${e.message}`))
        }
      />
      <LearnPanel />

      {pending?.kind === 'delete' && (
        <ConfirmDialog
          title="Delete this Pod?"
          confirmLabel="Delete Pod"
          danger
          onCancel={() => setPending(null)}
          onConfirm={() => {
            const p = pending;
            setPending(null);
            void run(() => api.deletePod(p.name));
          }}
        >
          <p>
            KubeScope will delete <code>{pending.name}</code> (uid <code>{pending.uid}</code>) in namespace{' '}
            <code>{c?.namespace}</code> on context <code>{c?.context}</code>.
          </p>
          <p>The ReplicaSet should create a replacement Pod with a new UID. KubeScope will time how long recovery takes.</p>
          {c?.isGKE && <p className="warning">This is a GKE cluster.</p>}
        </ConfirmDialog>
      )}
      {pending?.kind === 'reset' && (
        <ConfirmDialog
          title="Reset to baseline?"
          confirmLabel="Reset"
          onCancel={() => setPending(null)}
          onConfirm={() => {
            setPending(null);
            void run(() => api.reset());
          }}
        >
          <p>
            Scale <code>{deployment}</code> to the baseline of {c?.baselineReplicas ?? 2} replicas on context{' '}
            <code>{c?.context}</code>.
          </p>
        </ConfirmDialog>
      )}
      <footer className="muted small">
        KubeScope is a client of the public Kubernetes API. It does not see inside the control plane; all timings are measured on this machine.
      </footer>
    </div>
  );
}
