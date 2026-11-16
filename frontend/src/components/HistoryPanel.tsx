import { api } from '../api/client';
import type { ExperimentSummary } from '../api/types';
import { fmtDateTime, fmtMs } from './format';

const KEY: Record<string, string> = {
  scale: 'time_to_convergence',
  'delete-pod': 'time_to_desired_readiness',
};

export function HistoryPanel({ history, onOpen }: { history: ExperimentSummary[]; onOpen: (id: string) => void }) {
  return (
    <section className="panel" aria-labelledby="hist-h">
      <div className="panel-head">
        <h2 id="hist-h">History (last 20)</h2>
        {history.length > 0 && <a className="btn ghost small" href={api.exportUrl()} download>Export all JSON</a>}
      </div>
      <p className="muted small">Kept in memory by the backend; restarting it clears history. Export to keep results.</p>
      {history.length === 0 ? (
        <div className="empty">No experiments yet.</div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Started</th>
                <th scope="col">Type</th>
                <th scope="col">Target</th>
                <th scope="col">Status</th>
                <th scope="col">API RTT</th>
                <th scope="col">To ready</th>
                <th scope="col">Context</th>
                <th scope="col"><span className="sr-only">Open</span></th>
              </tr>
            </thead>
            <tbody>
              {history.map((h) => {
                const rtt = h.measurements.find((m) => m.name === 'api_rtt');
                const main = h.measurements.find((m) => m.name === KEY[h.type]);
                return (
                  <tr key={h.id}>
                    <td>{fmtDateTime(h.createdAt)}</td>
                    <td>{h.type}{h.target.reset ? ' (reset)' : ''}</td>
                    <td>{h.type === 'scale' ? `${h.target.fromReplicas} → ${h.target.toReplicas}` : h.target.podName}</td>
                    <td><span className={`badge status-${h.status}`}>{h.status}</span></td>
                    <td>{fmtMs(rtt?.valueMs)}</td>
                    <td title={main?.reason}>{main?.valueMs === null || !main ? <span className="muted">n/a</span> : fmtMs(main.valueMs)}</td>
                    <td><code>{h.context}</code></td>
                    <td><button className="btn ghost small" onClick={() => onOpen(h.id)}>Details</button></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
