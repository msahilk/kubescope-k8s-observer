// Package models defines the JSON DTOs shared by the HTTP API and the frontend.
// Keep frontend/src/api/types.ts in sync with this file.
package models

import "time"

// Error codes returned in {"error":{"code":...}}.
const (
	CodeForbidden       = "FORBIDDEN"
	CodeInvalidArgument = "INVALID_ARGUMENT"
	CodeNotConnected    = "NOT_CONNECTED"
	CodeConflict        = "CONFLICT"
	CodeTimeout         = "TIMEOUT"
	CodeInternal        = "INTERNAL"
	CodeNotFound        = "NOT_FOUND"
	CodeRateLimited     = "RATE_LIMITED"
)

// ErrorBody is the JSON error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a machine code and a human message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Connection describes cluster connectivity. It never contains credentials.
type Connection struct {
	Configured       bool       `json:"configured"`
	Reachable        bool       `json:"reachable"`
	Context          string     `json:"context"`
	Cluster          string     `json:"cluster"`
	Server           string     `json:"server"`
	ServerVersion    string     `json:"serverVersion,omitempty"`
	Namespace        string     `json:"namespace"`
	Deployment       string     `json:"deployment"`
	IsGKE            bool       `json:"isGKE"`
	LastSuccessAt    *time.Time `json:"lastSuccessAt,omitempty"`
	LastCheckedAt    *time.Time `json:"lastCheckedAt,omitempty"`
	Error            string     `json:"error,omitempty"`
	WatchSynced      bool       `json:"watchSynced"`
	WatchErrors      int        `json:"watchErrors"`
	WatchRestarts    int        `json:"watchRestarts"`
	LastWatchError   string     `json:"lastWatchError,omitempty"`
	LastWatchErrorAt *time.Time `json:"lastWatchErrorAt,omitempty"`
	MinReplicas      int32      `json:"minReplicas"`
	MaxReplicas      int32      `json:"maxReplicas"`
	BaselineReplicas int32      `json:"baselineReplicas"`
	TimeoutSeconds   int        `json:"timeoutSeconds"`
}

// DeploymentInfo summarizes the demo Deployment.
type DeploymentInfo struct {
	Name               string `json:"name"`
	UID                string `json:"uid"`
	ResourceVersion    string `json:"resourceVersion"`
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Desired            int32  `json:"desired"`
	Current            int32  `json:"current"`
	Updated            int32  `json:"updated"`
	Ready              int32  `json:"ready"`
	Available          int32  `json:"available"`
	Unavailable        int32  `json:"unavailable"`
	Image              string `json:"image,omitempty"`
	Converged          bool   `json:"converged"`
	ConvergenceReason  string `json:"convergenceReason"`
}

// ReplicaSetInfo summarizes a ReplicaSet owned by the demo Deployment.
type ReplicaSetInfo struct {
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Revision        string `json:"revision,omitempty"`
	Desired         int32  `json:"desired"`
	Current         int32  `json:"current"`
	Ready           int32  `json:"ready"`
	Available       int32  `json:"available"`
}

// PodInfo summarizes a demo Pod. Running (container started) and Ready
// (readiness probe passing, PodReady condition) are reported separately.
type PodInfo struct {
	Name            string     `json:"name"`
	UID             string     `json:"uid"`
	ResourceVersion string     `json:"resourceVersion"`
	Phase           string     `json:"phase"`
	Running         bool       `json:"running"`
	Ready           bool       `json:"ready"`
	Terminating     bool       `json:"terminating"`
	NodeName        string     `json:"nodeName,omitempty"`
	ReplicaSet      string     `json:"replicaSet,omitempty"`
	Restarts        int32      `json:"restarts"`
	CreatedAt       time.Time  `json:"createdAt"`
	ReadySince      *time.Time `json:"readySince,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// EventInfo is a Kubernetes Event (core/v1) in the demo namespace.
type EventInfo struct {
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	InvolvedKind   string    `json:"involvedKind"`
	InvolvedName   string    `json:"involvedName"`
	Count          int32     `json:"count"`
	Source         string    `json:"source,omitempty"`
	LastObservedAt time.Time `json:"lastObservedAt"`
}

// Snapshot is a point-in-time listing fetched directly from the API server.
type Snapshot struct {
	ObservedAt  time.Time        `json:"observedAt"`
	Namespace   string           `json:"namespace"`
	Deployment  *DeploymentInfo  `json:"deployment"`
	ReplicaSets []ReplicaSetInfo `json:"replicaSets"`
	Pods        []PodInfo        `json:"pods"`
	Events      []EventInfo      `json:"events"`
	// StreamSeq is the latest SSE sequence id at snapshot time; the client can
	// discard buffered stream events with seq <= StreamSeq.
	StreamSeq uint64   `json:"streamSeq"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Experiment types and statuses.
const (
	TypeScale     = "scale"
	TypeDeletePod = "delete-pod"

	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusTimeout     = "timeout"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Timeline entry kinds. "inference" entries are KubeScope's explanation, not
// something the cluster reported.
const (
	KindAPIRequest  = "api_request"
	KindAPIResponse = "api_response"
	KindWatch       = "watch"
	KindPoll        = "poll"
	KindK8sEvent    = "k8s_event"
	KindObservation = "observation"
	KindInference   = "inference"
	KindStatus      = "status"
)

// TimelineEntry is one timestamped line in an experiment's timeline.
type TimelineEntry struct {
	At       time.Time `json:"at"`
	OffsetMs int64     `json:"offsetMs"`
	Kind     string    `json:"kind"`
	Observed bool      `json:"observed"`
	Resource string    `json:"resource,omitempty"`
	Message  string    `json:"message"`
}

// Measurement is one duration with explicit start/end conditions. ValueMs is
// nil when it could not be measured; Reason then explains why.
type Measurement struct {
	Name        string     `json:"name"`
	Label       string     `json:"label"`
	ValueMs     *int64     `json:"valueMs"`
	Start       string     `json:"start"`
	End         string     `json:"end"`
	StartAt     *time.Time `json:"startAt,omitempty"`
	EndAt       *time.Time `json:"endAt,omitempty"`
	Description string     `json:"description"`
	Reason      string     `json:"reason,omitempty"`
}

// Target identifies what an experiment acted on.
type Target struct {
	Namespace        string `json:"namespace"`
	Deployment       string `json:"deployment"`
	FromReplicas     int32  `json:"fromReplicas,omitempty"`
	ToReplicas       int32  `json:"toReplicas,omitempty"`
	PodName          string `json:"podName,omitempty"`
	PodUID           string `json:"podUid,omitempty"`
	ReplacementPod   string `json:"replacementPod,omitempty"`
	ReplacementUID   string `json:"replacementUid,omitempty"`
	BaselineReady    int32  `json:"baselineReady,omitempty"`
	BaselineDesired  int32  `json:"baselineDesired,omitempty"`
	MinReadyObserved *int32 `json:"minReadyObserved,omitempty"`
	Reset            bool   `json:"reset,omitempty"`
}

// Experiment is the full record of one run.
type Experiment struct {
	ID                    string          `json:"id"`
	Type                  string          `json:"type"`
	Status                string          `json:"status"`
	StatusReason          string          `json:"statusReason,omitempty"`
	Context               string          `json:"context"`
	Cluster               string          `json:"cluster"`
	IsGKE                 bool            `json:"isGKE"`
	Target                Target          `json:"target"`
	CreatedAt             time.Time       `json:"createdAt"`
	RequestStartedAt      *time.Time      `json:"requestStartedAt,omitempty"`
	RequestAcknowledgedAt *time.Time      `json:"requestAcknowledgedAt,omitempty"`
	ConvergedAt           *time.Time      `json:"convergedAt,omitempty"`
	FinishedAt            *time.Time      `json:"finishedAt,omitempty"`
	TimeoutSeconds        int             `json:"timeoutSeconds"`
	Measurements          []Measurement   `json:"measurements"`
	Timeline              []TimelineEntry `json:"timeline"`
	TimelineTruncated     bool            `json:"timelineTruncated,omitempty"`
	Caveats               []string        `json:"caveats"`
}

// ExperimentSummary is the compact history row.
type ExperimentSummary struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Status       string        `json:"status"`
	StatusReason string        `json:"statusReason,omitempty"`
	Context      string        `json:"context"`
	Cluster      string        `json:"cluster"`
	Target       Target        `json:"target"`
	CreatedAt    time.Time     `json:"createdAt"`
	FinishedAt   *time.Time    `json:"finishedAt,omitempty"`
	Measurements []Measurement `json:"measurements"`
}

// ScaleRequest is POST /experiments/scale.
type ScaleRequest struct {
	Deployment string `json:"deployment"`
	Replicas   *int32 `json:"replicas"`
}

// DeletePodRequest is POST /experiments/delete-pod.
type DeletePodRequest struct {
	PodName string `json:"podName"`
	Confirm bool   `json:"confirm"`
}

// StreamEvent is one SSE message.
type StreamEvent struct {
	Seq  uint64    `json:"seq"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

// Stream event types.
const (
	StreamResource   = "resource"   // a watch observation (Deployment/ReplicaSet/Pod)
	StreamK8sEvent   = "k8s_event"  // a Kubernetes Event
	StreamExperiment = "experiment" // experiment state update (full Experiment)
	StreamConnection = "connection" // connectivity change
	StreamResync     = "resync"     // client must refetch /snapshot
)

// ResourceChange is the payload of a "resource" stream event.
type ResourceChange struct {
	Kind            string          `json:"kind"`
	Action          string          `json:"action"` // ADDED|MODIFIED|DELETED
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Summary         string          `json:"summary"`
	Deployment      *DeploymentInfo `json:"deployment,omitempty"`
	Pod             *PodInfo        `json:"pod,omitempty"`
	ReplicaSet      *ReplicaSetInfo `json:"replicaSet,omitempty"`
}
