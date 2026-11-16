import type { Connection, Experiment, ExperimentSummary, Snapshot, ErrorCode } from './types';

const BASE = '/api/v1';

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: ErrorCode | 'NETWORK',
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(BASE + path, {
      ...init,
      headers: { Accept: 'application/json', ...(init?.body ? { 'Content-Type': 'application/json' } : {}) },
    });
  } catch (e) {
    throw new ApiError(0, 'NETWORK', 'KubeScope backend is unreachable. Is `go run ./cmd/server` running?');
  }
  const text = await res.text();
  let body: unknown = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    /* non-JSON (e.g. proxy error page) */
  }
  if (!res.ok) {
    const err = (body as { error?: { code: ErrorCode; message: string } })?.error;
    if (err) throw new ApiError(res.status, err.code, err.message);
    // The Go backend always answers errors with a JSON body. A 5xx without one
    // comes from the Vite dev proxy (it returns a bare 500 when the backend
    // refuses the connection), i.e. the backend itself is down.
    if (res.status >= 500) {
      throw new ApiError(res.status, 'NETWORK',
        `KubeScope backend is unreachable (dev proxy returned HTTP ${res.status}). Is \`go run ./cmd/server\` running?`);
    }
    throw new ApiError(res.status, 'INTERNAL', `HTTP ${res.status}`);
  }
  return body as T;
}

export const api = {
  connection: (refresh = false) => request<Connection>(`/connection${refresh ? '?refresh=true' : ''}`),
  snapshot: () => request<Snapshot>('/snapshot'),
  scale: (deployment: string, replicas: number) =>
    request<Experiment>('/experiments/scale', { method: 'POST', body: JSON.stringify({ deployment, replicas }) }),
  reset: () => request<Experiment>('/experiments/reset', { method: 'POST', body: JSON.stringify({ confirm: true }) }),
  deletePod: (podName: string) =>
    request<Experiment>('/experiments/delete-pod', { method: 'POST', body: JSON.stringify({ podName, confirm: true }) }),
  cancel: (id: string) => request<Experiment>(`/experiments/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: '{}' }),
  experiments: () => request<{ experiments: ExperimentSummary[] }>('/experiments'),
  experiment: (id: string) => request<Experiment>(`/experiments/${encodeURIComponent(id)}`),
  exportUrl: (id?: string) => (id ? `${BASE}/experiments/${encodeURIComponent(id)}/export` : `${BASE}/experiments/export`),
  streamUrl: `${BASE}/events/stream`,
};
