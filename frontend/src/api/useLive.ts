import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError } from './client';
import type { Connection, Experiment, ExperimentSummary, PodInfo, ResourceChange, Snapshot, StreamEvent } from './types';

export type StreamState = 'connecting' | 'open' | 'reconnecting' | 'closed';

export interface FeedItem {
  seq: number;
  at: string;
  kind: 'resource' | 'k8s_event' | 'stream';
  title: string;
  detail: string;
  observed: boolean;
}

export interface LiveState {
  connection: Connection | null;
  connectionError: ApiError | null;
  snapshot: Snapshot | null;
  snapshotError: ApiError | null;
  snapshotLoading: boolean;
  lastRefresh: Date | null;
  stream: StreamState;
  feed: FeedItem[];
  current: Experiment | null;
  history: ExperimentSummary[];
  refresh: () => Promise<void>;
  setCurrent: (e: Experiment | null) => void;
  reloadHistory: () => Promise<void>;
}

const FEED_MAX = 200;

function upsertPod(pods: PodInfo[], p: PodInfo): PodInfo[] {
  const i = pods.findIndex((x) => x.uid === p.uid);
  const next = i >= 0 ? pods.map((x, j) => (j === i ? p : x)) : [...pods, p];
  return next.sort((a, b) => a.createdAt.localeCompare(b.createdAt) || a.name.localeCompare(b.name));
}

function applyChange(s: Snapshot, c: ResourceChange): Snapshot {
  switch (c.kind) {
    case 'Deployment':
      return { ...s, deployment: c.action === 'DELETED' ? null : c.deployment ?? s.deployment };
    case 'Pod':
      if (c.action === 'DELETED') return { ...s, pods: s.pods.filter((p) => p.uid !== c.uid) };
      return c.pod ? { ...s, pods: upsertPod(s.pods, c.pod) } : s;
    case 'ReplicaSet': {
      if (c.action === 'DELETED') return { ...s, replicaSets: s.replicaSets.filter((r) => r.uid !== c.uid) };
      if (!c.replicaSet) return s;
      const others = s.replicaSets.filter((r) => r.uid !== c.uid);
      return { ...s, replicaSets: [...others, c.replicaSet] };
    }
  }
  return s;
}

/**
 * useLive keeps an initial REST snapshot up to date with the SSE stream.
 * On reconnect or an explicit "resync" message it refetches the snapshot, so a
 * dropped stream can never leave the UI permanently inconsistent.
 */
export function useLive(): LiveState {
  const [connection, setConnection] = useState<Connection | null>(null);
  const [connectionError, setConnectionError] = useState<ApiError | null>(null);
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [snapshotError, setSnapshotError] = useState<ApiError | null>(null);
  const [snapshotLoading, setSnapshotLoading] = useState(true);
  const [lastRefresh, setLastRefresh] = useState<Date | null>(null);
  const [stream, setStream] = useState<StreamState>('connecting');
  const [feed, setFeed] = useState<FeedItem[]>([]);
  const [current, setCurrent] = useState<Experiment | null>(null);
  const [history, setHistory] = useState<ExperimentSummary[]>([]);
  const snapSeq = useRef(0);
  const inflight = useRef<Promise<void> | null>(null);

  const backendDown = useRef(false);
  // Returns true when the backend answered after previously failing, so the
  // caller can refetch the snapshot instead of waiting for the 30 s resync.
  const loadConnection = useCallback(async (): Promise<boolean> => {
    try {
      setConnection(await api.connection());
      setConnectionError(null);
      const recovered = backendDown.current;
      backendDown.current = false;
      return recovered;
    } catch (e) {
      setConnectionError(e as ApiError);
      backendDown.current = true;
      return false;
    }
  }, []);

  const refresh = useCallback(async () => {
    if (inflight.current) return inflight.current;
    const p = (async () => {
      setSnapshotLoading(true);
      try {
        const s = await api.snapshot();
        snapSeq.current = s.streamSeq;
        setSnapshot(s);
        setSnapshotError(null);
        setLastRefresh(new Date());
      } catch (e) {
        setSnapshotError(e as ApiError);
        // Never keep showing stale data as if it were current.
        setSnapshot(null);
      } finally {
        setSnapshotLoading(false);
        inflight.current = null;
      }
      void loadConnection();
    })();
    inflight.current = p;
    return p;
  }, [loadConnection]);

  const reloadHistory = useCallback(async () => {
    try {
      const list = (await api.experiments()).experiments;
      setHistory(list);
      // After a page reload, re-attach to an experiment that is still running.
      if (list[0]?.status === 'running') {
        const x = await api.experiment(list[0].id);
        setCurrent((cur) => cur ?? x);
      }
    } catch {
      /* history is non-critical; errors surface via connection state */
    }
  }, []);

  useEffect(() => {
    void refresh();
    void reloadHistory();
    const t = window.setInterval(() => {
      void loadConnection().then((recovered) => {
        if (recovered) void refresh();
      });
    }, 10_000);
    const t2 = window.setInterval(() => void refresh(), 30_000); // belt-and-braces resync
    return () => {
      window.clearInterval(t);
      window.clearInterval(t2);
    };
  }, [refresh, reloadHistory, loadConnection]);

  useEffect(() => {
    let es: EventSource | null = null;
    let retryTimer: number | undefined;
    let attempts = 0;
    let hadError = false;
    let disposed = false;

    const pushFeed = (items: FeedItem[]) =>
      setFeed((f) => [...items, ...f].slice(0, FEED_MAX));

    const onMessage = (msg: MessageEvent<string>) => {
      let ev: StreamEvent;
      try {
        ev = JSON.parse(msg.data) as StreamEvent;
      } catch {
        return;
      }
      switch (ev.type) {
        case 'hello':
          setConnection(ev.data.connection);
          break;
        case 'resync':
          pushFeed([{ seq: 0, at: ev.at, kind: 'stream', title: 'Stream resync', detail: ev.data.reason, observed: false }]);
          void refresh();
          break;
        case 'connection':
          void loadConnection();
          break;
        case 'resource': {
          const c = ev.data;
          pushFeed([{ seq: ev.seq, at: ev.at, kind: 'resource', title: `${c.kind} ${c.action}`, detail: `${c.name}: ${c.summary}`, observed: true }]);
          if (ev.seq > snapSeq.current) setSnapshot((s) => (s ? applyChange(s, c) : s));
          break;
        }
        case 'k8s_event': {
          const e = ev.data;
          pushFeed([{ seq: ev.seq, at: ev.at, kind: 'k8s_event', title: `Event ${e.type} ${e.reason}`, detail: `${e.involvedKind}/${e.involvedName}: ${e.message}`, observed: true }]);
          if (ev.seq > snapSeq.current) setSnapshot((s) => (s ? { ...s, events: [e, ...s.events].slice(0, 50) } : s));
          break;
        }
        case 'experiment': {
          const x = ev.data;
          setCurrent((cur) => {
            if (cur && cur.id === x.id && cur.timeline.length > x.timeline.length) return cur;
            return !cur || cur.id === x.id || cur.status !== 'running' ? x : cur;
          });
          if (x.status !== 'running') {
            void reloadHistory();
            void refresh();
          }
          break;
        }
      }
    };

    const connect = () => {
      if (disposed) return;
      setStream(attempts === 0 ? 'connecting' : 'reconnecting');
      es = new EventSource(api.streamUrl);
      es.onopen = () => {
        attempts = 0;
        setStream('open');
        if (hadError) {
          hadError = false;
          void refresh(); // events may have been missed while disconnected
        }
      };
      es.onmessage = onMessage;
      es.onerror = () => {
        hadError = true;
        if (es && es.readyState === EventSource.CLOSED) {
          // Browser gave up (e.g. HTTP error): reconnect ourselves with backoff.
          // Capped at 5 s: the backend is a local process, retries are cheap.
          es.close();
          attempts++;
          setStream('reconnecting');
          retryTimer = window.setTimeout(connect, Math.min(5_000, 1000 * 2 ** Math.min(attempts, 3)));
        } else {
          setStream('reconnecting'); // native EventSource retry with Last-Event-ID
        }
      };
    };
    connect();
    return () => {
      disposed = true;
      window.clearTimeout(retryTimer);
      es?.close();
      setStream('closed');
    };
  }, [refresh, reloadHistory, loadConnection]);

  // Merge so an older copy of the same experiment (e.g. the POST response that
  // raced with a newer SSE update) never overwrites a newer one.
  const mergeCurrent = useCallback((x: Experiment | null) => {
    setCurrent((cur) => {
      if (x && cur && cur.id === x.id && (cur.status !== 'running' && x.status === 'running' || cur.timeline.length > x.timeline.length)) {
        return cur;
      }
      return x;
    });
  }, []);

  return {
    connection,
    connectionError,
    snapshot,
    snapshotError,
    snapshotLoading,
    lastRefresh,
    stream,
    feed,
    current,
    history,
    refresh,
    setCurrent: mergeCurrent,
    reloadHistory,
  };
}
