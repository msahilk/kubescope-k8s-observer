// Mirrors backend/internal/models/models.go. Keep the two in sync.

export type ErrorCode =
  | 'FORBIDDEN'
  | 'INVALID_ARGUMENT'
  | 'NOT_CONNECTED'
  | 'CONFLICT'
  | 'TIMEOUT'
  | 'INTERNAL'
  | 'NOT_FOUND'
  | 'RATE_LIMITED';

export interface Connection {
  configured: boolean;
  reachable: boolean;
  context: string;
  cluster: string;
  server: string;
  serverVersion?: string;
  namespace: string;
  deployment: string;
  isGKE: boolean;
  lastSuccessAt?: string;
  lastCheckedAt?: string;
  error?: string;
  watchSynced: boolean;
  watchErrors: number;
  watchRestarts: number;
  lastWatchError?: string;
  lastWatchErrorAt?: string;
  minReplicas: number;
  maxReplicas: number;
  baselineReplicas: number;
  timeoutSeconds: number;
}

export interface DeploymentInfo {
  name: string;
  uid: string;
  resourceVersion: string;
  generation: number;
  observedGeneration: number;
  desired: number;
  current: number;
  updated: number;
  ready: number;
  available: number;
  unavailable: number;
  image?: string;
  converged: boolean;
  convergenceReason: string;
}

export interface ReplicaSetInfo {
  name: string;
  uid: string;
  resourceVersion: string;
  revision?: string;
  desired: number;
  current: number;
  ready: number;
  available: number;
}

export interface PodInfo {
  name: string;
  uid: string;
  resourceVersion: string;
  phase: string;
  running: boolean;
  ready: boolean;
  terminating: boolean;
  nodeName?: string;
  replicaSet?: string;
  restarts: number;
  createdAt: string;
  readySince?: string;
  reason?: string;
}

export interface EventInfo {
  type: string;
  reason: string;
  message: string;
  involvedKind: string;
  involvedName: string;
  count: number;
  source?: string;
  lastObservedAt: string;
}

export interface Snapshot {
  observedAt: string;
  namespace: string;
  deployment: DeploymentInfo | null;
  replicaSets: ReplicaSetInfo[];
  pods: PodInfo[];
  events: EventInfo[];
  streamSeq: number;
  warnings?: string[];
}

export type ExperimentStatus = 'running' | 'success' | 'timeout' | 'failed' | 'interrupted';
export type ExperimentType = 'scale' | 'delete-pod';

export interface TimelineEntry {
  at: string;
  offsetMs: number;
  kind: 'api_request' | 'api_response' | 'watch' | 'poll' | 'k8s_event' | 'observation' | 'inference' | 'status';
  observed: boolean;
  resource?: string;
  message: string;
}

export interface Measurement {
  name: string;
  label: string;
  valueMs: number | null;
  start: string;
  end: string;
  startAt?: string;
  endAt?: string;
  description: string;
  reason?: string;
}

export interface Target {
  namespace: string;
  deployment: string;
  fromReplicas?: number;
  toReplicas?: number;
  podName?: string;
  podUid?: string;
  replacementPod?: string;
  replacementUid?: string;
  baselineReady?: number;
  baselineDesired?: number;
  minReadyObserved?: number;
  reset?: boolean;
}

export interface Experiment {
  id: string;
  type: ExperimentType;
  status: ExperimentStatus;
  statusReason?: string;
  context: string;
  cluster: string;
  isGKE: boolean;
  target: Target;
  createdAt: string;
  requestStartedAt?: string;
  requestAcknowledgedAt?: string;
  convergedAt?: string;
  finishedAt?: string;
  timeoutSeconds: number;
  measurements: Measurement[];
  timeline: TimelineEntry[];
  timelineTruncated?: boolean;
  caveats: string[];
}

export interface ExperimentSummary {
  id: string;
  type: ExperimentType;
  status: ExperimentStatus;
  statusReason?: string;
  context: string;
  cluster: string;
  target: Target;
  createdAt: string;
  finishedAt?: string;
  measurements: Measurement[];
}

export interface ResourceChange {
  kind: 'Deployment' | 'ReplicaSet' | 'Pod';
  action: 'ADDED' | 'MODIFIED' | 'DELETED';
  name: string;
  uid: string;
  resourceVersion: string;
  summary: string;
  deployment?: DeploymentInfo;
  pod?: PodInfo;
  replicaSet?: ReplicaSetInfo;
}

export type StreamEvent =
  | { seq: number; type: 'resource'; at: string; data: ResourceChange }
  | { seq: number; type: 'k8s_event'; at: string; data: EventInfo }
  | { seq: number; type: 'experiment'; at: string; data: Experiment }
  | { seq: number; type: 'connection'; at: string; data: { reachable: boolean; error?: string } }
  | { seq: number; type: 'resync'; at: string; data: { reason: string } }
  | { seq: number; type: 'hello'; at: string; data: { seq: number; connection: Connection } };
