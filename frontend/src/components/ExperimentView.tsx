import type { Experiment, TimelineEntry } from '../api/types';
import { api } from '../api/client';
import { fmtMs, fmtTime, shortUid } from './format';

const KIND_LABEL: Record<TimelineEntry['kind'], string> = {
  api_request: 'API request',
  api_response: 'API response',
  watch: 'Watch',
  poll: 'Poll',
  k8s_event: 'K8s Event',
  observation: 'Observed',
  inference: 'Explanation',
  status: 'Status',
};

export function ExperimentView({ exp }: { exp: Experiment | null }) {
  if (!exp) {
    return (
      <section className="panel" aria-labelledby="exp-h">
        <h2 id="exp-h">Current experiment</h2>
        <div className="empty">No experiment yet. Scale the Deployment or delete a Pod to start one.</div>
      </section>
    );
  }
  const t = exp.target;
  return (
    <section className="panel" aria-labelledby="exp-h" aria-live="polite">
      <div className="panel-head">
        <h2 id="exp-h">
          {exp.type === 'scale' ? (t.reset ? 'Reset' : 'Scale') : 'Pod recovery'}{' '}
          <span className={`badge status-${exp.status}`}>{exp.status}</span>
        </h2>
        {exp.status !== 'running' && (
          <a className="btn ghost small" href={api.exportUrl(exp.id)} download>Export JSON</a>
        )}
      </div>
      <p className="muted small">
        <code>{exp.id}</code> · context <code>{exp.context}</code>
        {exp.type === 'scale' ? (
          <> · replicas {t.fromReplicas} → {t.toReplicas}</>
        ) : (
          <>
            {' '}· deleted <code>{t.podName}</code> (uid {shortUid(t.podUid)})
            {t.replacementPod && <> → replacement <code>{t.replacementPod}</code> (uid {shortUid(t.replacementUid)})</>}
            {' '}· baseline ready {t.baselineReady}/{t.baselineDesired}
            {t.minReadyObserved !== undefined && <> · min ready observed {t.minReadyObserved}</>}
          </>
        )}
      </p>
      {exp.statusReason && <p className={exp.status === 'success' ? 'ok-text' : exp.status === 'running' ? '' : 'error-text'}>{exp.statusReason}</p>}

      <h3>Measurements</h3>
      <div className="metrics">
        {exp.measurements.map((m) => (
          <div className="metric" key={m.name}>
            <div className="metric-value">{m.valueMs === null ? <span className="muted">not measured</span> : fmtMs(m.valueMs)}</div>
            <div className="metric-label">{m.label}</div>
            <details>
              <summary>What does this measure?</summary>
              <p><strong>Start:</strong> {m.start}</p>
              <p><strong>End:</strong> {m.end}</p>
              <p>{m.description}</p>
              {m.reason && <p className="muted"><strong>Why missing:</strong> {m.reason}</p>}
            </details>
          </div>
        ))}
      </div>

      <h3>Timeline</h3>
      <ol className="timeline">
        {exp.timeline.map((e, i) => (
          <li key={i} className={`tl tl-${e.kind}`}>
            <span className="tl-time">{e.offsetMs >= 0 ? '+' : '−'}{Math.abs(e.offsetMs)} ms<br /><span className="muted">{fmtTime(e.at)}</span></span>
            <span className={`tl-kind ${e.observed ? '' : 'inferred'}`}>{KIND_LABEL[e.kind]}</span>
            <span className="tl-msg">
              {e.resource && <code>{e.resource}</code>} {e.message}
            </span>
          </li>
        ))}
      </ol>
      {exp.timelineTruncated && <p className="muted small">Timeline truncated (too many entries).</p>}
      <p className="legend small muted">
        <span className="tl-kind">Observed</span> entries come from API responses, watches, polls or Kubernetes Events.{' '}
        <span className="tl-kind inferred">Explanation</span> entries are KubeScope's description of what Kubernetes is expected to do — not observations.
      </p>
      <h3>Caveats</h3>
      <ul className="caveats">
        {exp.caveats.map((c) => <li key={c}>{c}</li>)}
      </ul>
    </section>
  );
}
