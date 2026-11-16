import type { EventInfo } from '../api/types';
import type { FeedItem } from '../api/useLive';
import { fmtTime } from './format';

export function LiveFeed({ feed, events }: { feed: FeedItem[]; events: EventInfo[] }) {
  return (
    <section className="panel" aria-labelledby="feed-h">
      <h2 id="feed-h">Live cluster feed</h2>
      <p className="muted small">Raw watch changes and Kubernetes Events as KubeScope receives them (newest first).</p>
      {feed.length === 0 ? (
        <div className="empty">Waiting for changes… (an idle cluster produces no watch events)</div>
      ) : (
        <ol className="feed">
          {feed.map((f, i) => (
            <li key={`${f.seq}-${i}`} className={`feed-${f.kind}`}>
              <span className="tl-time">{fmtTime(f.at)}</span>
              <span><strong>{f.title}</strong> <span className="feed-detail">{f.detail}</span></span>
            </li>
          ))}
        </ol>
      )}
      {events.length > 0 && (
        <details>
          <summary>Recent Kubernetes Events in the namespace ({events.length})</summary>
          <ul className="feed">
            {events.map((e, i) => (
              <li key={i}>
                <span className="tl-time">{fmtTime(e.lastObservedAt)}</span>
                <span><strong>{e.type} {e.reason}</strong> <span className="feed-detail">{e.involvedKind}/{e.involvedName}: {e.message}{e.count > 1 ? ` (×${e.count})` : ''}</span></span>
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  );
}
