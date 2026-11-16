import { useEffect, useState } from 'react';
import type { Connection, Experiment, Snapshot } from '../api/types';

interface Props {
  connection: Connection | null;
  snapshot: Snapshot | null;
  current: Experiment | null;
  busy: boolean;
  canMutate: boolean;
  onScale: (n: number) => void;
  onReset: () => void;
  onCancel: (id: string) => void;
}

export function ExperimentControls({ connection, snapshot, current, busy, canMutate, onScale, onReset, onCancel }: Props) {
  const min = connection?.minReplicas ?? 1;
  const max = connection?.maxReplicas ?? 5;
  const desired = snapshot?.deployment?.desired;
  const [n, setN] = useState<number>(desired ?? 2);
  useEffect(() => {
    if (desired !== undefined) setN((cur) => (cur === desired ? (desired >= max ? desired - 1 : desired + 1) : cur));
  }, [desired, max]);

  const running = current?.status === 'running';
  const valid = Number.isInteger(n) && n >= min && n <= max;
  const disabled = !canMutate || running || busy;

  return (
    <section className="panel" aria-labelledby="ctl-h">
      <h2 id="ctl-h">Experiments</h2>
      {!canMutate && <p className="muted">Controls are disabled until the cluster is reachable and the demo Deployment exists.</p>}
      <form
        className="scale-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (valid && !disabled) onScale(n);
        }}
      >
        <label htmlFor="replicas">Desired replicas ({min}–{max})</label>
        <div className="row">
          <input
            id="replicas"
            type="number"
            min={min}
            max={max}
            step={1}
            value={Number.isNaN(n) ? '' : n}
            onChange={(e) => setN(parseInt(e.target.value, 10))}
            aria-invalid={!valid}
          />
          <button className="btn primary" type="submit" disabled={disabled || !valid}>
            Scale {desired !== undefined && valid ? `${desired} → ${n}` : ''}
          </button>
        </div>
        {!valid && <p className="error-text">Enter a whole number between {min} and {max}.</p>}
      </form>
      <div className="row wrap">
        <button className="btn" onClick={onReset} disabled={disabled}>
          Reset to baseline ({connection?.baselineReplicas ?? 2})…
        </button>
        {running && current && (
          <button className="btn ghost" onClick={() => onCancel(current.id)}>Cancel running experiment</button>
        )}
      </div>
      <p className="muted small">
        Pod deletion: use <em>Delete…</em> on a Pod row. One experiment runs at a time; each times out after{' '}
        {connection?.timeoutSeconds ?? 120}s.
      </p>
    </section>
  );
}
