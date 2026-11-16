// Package experiments runs KubeScope's two mutating experiments (scale and
// delete-pod), records separately-timestamped phases, and evaluates
// convergence from observed state. Nothing in here fabricates a measurement:
// every duration has explicit start/end conditions and is nil when unobserved.
package experiments

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sahilkamil/kubescope/backend/internal/kube"
	"github.com/sahilkamil/kubescope/backend/internal/models"
	"github.com/sahilkamil/kubescope/backend/internal/safety"
)

// Errors returned by the engine.
var (
	ErrConflict     = errors.New("another experiment is already running for this deployment")
	ErrNotFound     = errors.New("experiment not found")
	ErrNotRunning   = errors.New("experiment is not running")
	ErrConfirmation = fmt.Errorf("%w: deleting a Pod requires \"confirm\": true", safety.ErrInvalid)
)

// Ops is the subset of the Kubernetes client the engine needs.
type Ops interface {
	GetDeployment(ctx context.Context) (*appsv1.Deployment, error)
	PatchReplicas(ctx context.Context, replicas int32) (*appsv1.Deployment, error)
	GetPod(ctx context.Context, name string) (*corev1.Pod, error)
	ListPods(ctx context.Context) ([]corev1.Pod, error)
	ReplicaSetOwner(ctx context.Context, rsName string) (string, error)
	DeletePod(ctx context.Context, name string, uid types.UID) error
}

// Publisher receives experiment updates (the SSE hub).
type Publisher interface {
	Publish(typ string, data any) models.StreamEvent
}

// Options configures an Engine.
type Options struct {
	Timeout      time.Duration
	PollInterval time.Duration
	HistorySize  int
	MaxTimeline  int
	Identity     kube.Identity
	Policy       safety.Policy
	Now          func() time.Time
}

// Engine coordinates experiments. One mutating experiment per Deployment.
type Engine struct {
	ops  Ops
	pub  Publisher
	opts Options

	mu      sync.Mutex
	running map[string]*run // keyed by deployment name
	history []*run          // oldest first
	counter atomic.Uint64
	closed  bool
}

// New creates an Engine.
func New(ops Ops, pub Publisher, opts Options) *Engine {
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.HistorySize <= 0 {
		opts.HistorySize = 20
	}
	if opts.MaxTimeline <= 0 {
		opts.MaxTimeline = 400
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Engine{ops: ops, pub: pub, opts: opts, running: map[string]*run{}}
}

// run is the mutable state of one experiment.
type run struct {
	e      *Engine
	mu     sync.Mutex
	exp    models.Experiment
	obsCh  chan kube.Observation
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	t0     time.Time

	// scale
	targetGen int64
	desired   int32

	// delete-pod
	podUID         types.UID
	baselineUIDs   map[types.UID]bool
	replacementUID types.UID
	terminatingAt  *time.Time
	goneAt         *time.Time
	replacementAt  *time.Time
	replacementRdy *time.Time
	dipAt          *time.Time
	minReady       *int32
	lastDeployment *appsv1.Deployment
	podStates      map[types.UID]string
	droppedObs     int
	lastPollErr    string
}

// ---------- public API ----------

// StartScale validates and starts a scale experiment. It returns once the PATCH
// has been acknowledged (or failed); convergence is observed asynchronously.
func (e *Engine) StartScale(ctx context.Context, deployment string, replicas int32, reset bool) (models.Experiment, error) {
	p := e.opts.Policy
	if err := p.ValidateDeploymentName(deployment); err != nil {
		return models.Experiment{}, err
	}
	if err := p.ValidateReplicas(replicas); err != nil {
		return models.Experiment{}, err
	}
	r, err := e.acquire(deployment, models.TypeScale)
	if err != nil {
		return models.Experiment{}, err
	}
	d, err := e.ops.GetDeployment(ctx)
	if err == nil {
		err = p.CheckDeployment(d)
	}
	if err != nil {
		e.release(r, false)
		return models.Experiment{}, err
	}
	from := kube.SpecReplicas(d)
	r.mu.Lock()
	r.desired = replicas
	r.exp.Target = models.Target{Namespace: p.Namespace, Deployment: deployment, FromReplicas: from, ToReplicas: replicas, Reset: reset}
	r.exp.Caveats = scaleCaveats(e.opts.Identity.IsGKE)
	if from == replicas {
		r.exp.Caveats = append(r.exp.Caveats, fmt.Sprintf("Desired replicas were already %d: the PATCH is a no-op and convergence reflects the existing state.", replicas))
	}
	if !kube.DeploymentConverged(d, from).Converged {
		r.exp.Caveats = append(r.exp.Caveats, "The Deployment was not converged when the experiment started ("+kube.DeploymentConverged(d, from).Reason+").")
	}
	r.addLocked(models.KindInference, false, "", fmt.Sprintf("Expected chain: API server persists spec.replicas=%d → Deployment controller resizes the ReplicaSet → ReplicaSet controller creates/deletes Pods → scheduler binds new Pods → kubelet starts containers → readiness probe passes.", replicas))
	started := e.opts.Now()
	r.setStartLocked(started)
	r.exp.RequestStartedAt = &started
	r.addAtLocked(started, models.KindAPIRequest, true, "Deployment/"+deployment, fmt.Sprintf("PATCH spec.replicas %d → %d (merge patch)", from, replicas))
	r.mu.Unlock()
	e.record(r)

	resp, err := e.ops.PatchReplicas(ctx, replicas)
	ack := e.opts.Now()
	r.mu.Lock()
	if err != nil {
		r.addLocked(models.KindAPIResponse, true, "Deployment/"+deployment, "PATCH failed: "+err.Error())
		r.finishLocked(models.StatusFailed, "PATCH failed: "+err.Error())
		r.mu.Unlock()
		e.release(r, true)
		return r.snapshot(), err
	}
	r.exp.RequestAcknowledgedAt = &ack
	r.targetGen = resp.Generation
	r.addLocked(models.KindAPIResponse, true, "Deployment/"+deployment,
		fmt.Sprintf("PATCH acknowledged in %d ms: generation=%d resourceVersion=%s", ack.Sub(started).Milliseconds(), resp.Generation, resp.ResourceVersion))
	r.computeLocked()
	r.mu.Unlock()
	e.publish(r)

	go r.loop(resp)
	return r.snapshot(), nil
}

// StartDeletePod validates and starts a Pod recovery experiment.
func (e *Engine) StartDeletePod(ctx context.Context, podName string, confirm bool) (models.Experiment, error) {
	p := e.opts.Policy
	if err := safety.ValidatePodName(podName); err != nil {
		return models.Experiment{}, err
	}
	if !confirm {
		return models.Experiment{}, ErrConfirmation
	}
	r, err := e.acquire(p.Deployment, models.TypeDeletePod)
	if err != nil {
		return models.Experiment{}, err
	}
	fail := func(err error) (models.Experiment, error) {
		e.release(r, false)
		return models.Experiment{}, err
	}
	pod, err := e.ops.GetPod(ctx, podName)
	if err != nil {
		return fail(err)
	}
	if err := p.CheckPodDeletable(pod, func(rs string) (string, error) { return e.ops.ReplicaSetOwner(ctx, rs) }); err != nil {
		return fail(err)
	}
	d, err := e.ops.GetDeployment(ctx)
	if err == nil {
		err = p.CheckDeployment(d)
	}
	if err != nil {
		return fail(err)
	}
	pods, err := e.ops.ListPods(ctx)
	if err != nil {
		return fail(err)
	}

	r.mu.Lock()
	r.podUID = pod.UID
	r.podStates = map[types.UID]string{}
	r.baselineUIDs = map[types.UID]bool{}
	for i := range pods {
		r.baselineUIDs[pods[i].UID] = true
	}
	r.baselineUIDs[pod.UID] = true
	desired := kube.SpecReplicas(d)
	r.desired = desired
	r.targetGen = d.Generation
	r.lastDeployment = d
	ready := d.Status.ReadyReplicas
	r.exp.Target = models.Target{Namespace: p.Namespace, Deployment: p.Deployment, PodName: pod.Name, PodUID: string(pod.UID), BaselineDesired: desired, BaselineReady: ready}
	r.exp.Caveats = deleteCaveats(e.opts.Identity.IsGKE)
	if c := kube.DeploymentConverged(d, desired); !c.Converged {
		r.exp.Caveats = append(r.exp.Caveats, "Baseline was not fully converged before deletion ("+c.Reason+"); recovery is measured against desired replicas.")
	}
	if ok, _ := kube.PodReady(pod); !ok {
		r.exp.Caveats = append(r.exp.Caveats, "The selected Pod was not Ready before deletion, so deleting it may not reduce ready replicas.")
	}
	r.addLocked(models.KindObservation, true, "Deployment/"+p.Deployment, fmt.Sprintf("Baseline: desired=%d ready=%d available=%d (%d Pods listed)", desired, ready, d.Status.AvailableReplicas, len(pods)))
	r.addLocked(models.KindInference, false, "", "Expected chain: API server marks the Pod terminating → kubelet stops it → ReplicaSet controller sees too few active Pods and creates a new Pod (new UID) → scheduler binds it → kubelet starts it → readiness probe passes.")
	started := e.opts.Now()
	r.setStartLocked(started)
	r.exp.RequestStartedAt = &started
	r.addAtLocked(started, models.KindAPIRequest, true, "Pod/"+pod.Name, fmt.Sprintf("DELETE pod %s (precondition uid=%s)", pod.Name, pod.UID))
	r.mu.Unlock()
	e.record(r)

	err = e.ops.DeletePod(ctx, pod.Name, pod.UID)
	ack := e.opts.Now()
	r.mu.Lock()
	if err != nil {
		r.addLocked(models.KindAPIResponse, true, "Pod/"+pod.Name, "DELETE failed: "+err.Error())
		r.finishLocked(models.StatusFailed, "DELETE failed: "+err.Error())
		r.mu.Unlock()
		e.release(r, true)
		return r.snapshot(), err
	}
	r.exp.RequestAcknowledgedAt = &ack
	r.addLocked(models.KindAPIResponse, true, "Pod/"+pod.Name, fmt.Sprintf("DELETE acknowledged in %d ms", ack.Sub(started).Milliseconds()))
	r.computeLocked()
	r.mu.Unlock()
	e.publish(r)

	go r.loop(nil)
	return r.snapshot(), nil
}

// Observe routes a watch observation to running experiments (non-blocking).
func (e *Engine) Observe(o kube.Observation) {
	e.mu.Lock()
	runs := make([]*run, 0, len(e.running))
	for _, r := range e.running {
		runs = append(runs, r)
	}
	e.mu.Unlock()
	for _, r := range runs {
		select {
		case r.obsCh <- o:
		default:
			r.mu.Lock()
			r.droppedObs++
			r.mu.Unlock()
		}
	}
}

// Cancel interrupts a running experiment.
func (e *Engine) Cancel(id string) (models.Experiment, error) {
	r := e.find(id)
	if r == nil {
		return models.Experiment{}, ErrNotFound
	}
	r.mu.Lock()
	running := r.exp.Status == models.StatusRunning
	r.mu.Unlock()
	if !running || r.cancel == nil {
		return r.snapshot(), ErrNotRunning
	}
	r.cancel(errCancelled)
	<-r.done
	return r.snapshot(), nil
}

// Get returns one experiment.
func (e *Engine) Get(id string) (models.Experiment, error) {
	r := e.find(id)
	if r == nil {
		return models.Experiment{}, ErrNotFound
	}
	return r.snapshot(), nil
}

// List returns history summaries, newest first.
func (e *Engine) List() []models.ExperimentSummary {
	e.mu.Lock()
	runs := append([]*run(nil), e.history...)
	e.mu.Unlock()
	out := make([]models.ExperimentSummary, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		x := runs[i].snapshot()
		out = append(out, models.ExperimentSummary{
			ID: x.ID, Type: x.Type, Status: x.Status, StatusReason: x.StatusReason, Context: x.Context,
			Cluster: x.Cluster, Target: x.Target, CreatedAt: x.CreatedAt, FinishedAt: x.FinishedAt, Measurements: x.Measurements,
		})
	}
	return out
}

// All returns full records (newest first), for bulk export.
func (e *Engine) All() []models.Experiment {
	e.mu.Lock()
	runs := append([]*run(nil), e.history...)
	e.mu.Unlock()
	out := make([]models.Experiment, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i].snapshot())
	}
	return out
}

// Close interrupts running experiments (server shutdown).
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	runs := make([]*run, 0, len(e.running))
	for _, r := range e.running {
		runs = append(runs, r)
	}
	e.mu.Unlock()
	for _, r := range runs {
		if r.cancel != nil {
			r.cancel(errShutdown)
			<-r.done
		}
	}
}

var (
	errCancelled = errors.New("cancelled by user")
	errShutdown  = errors.New("KubeScope server shutting down")
)

// ---------- internals ----------

func (e *Engine) acquire(deployment, typ string) (*run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errShutdown
	}
	if _, busy := e.running[deployment]; busy {
		return nil, ErrConflict
	}
	now := e.opts.Now()
	n := e.counter.Add(1)
	ctx, cancel := context.WithCancelCause(context.Background())
	r := &run{
		e:      e,
		obsCh:  make(chan kube.Observation, 1024),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		t0:     now,
		exp: models.Experiment{
			ID:             fmt.Sprintf("exp-%s-%d", now.Format("20060102-150405"), n),
			Type:           typ,
			Status:         models.StatusRunning,
			Context:        e.opts.Identity.Context,
			Cluster:        e.opts.Identity.Cluster,
			IsGKE:          e.opts.Identity.IsGKE,
			CreatedAt:      now,
			TimeoutSeconds: int(e.opts.Timeout / time.Second),
			Measurements:   []models.Measurement{},
			Timeline:       []models.TimelineEntry{},
			Caveats:        []string{},
		},
	}
	e.running[deployment] = r
	return r, nil
}

// release frees the deployment lock; keep=true records the run in history.
func (e *Engine) release(r *run, keep bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.running[r.exp.Target.Deployment]; ok && cur == r {
		delete(e.running, r.exp.Target.Deployment)
	}
	for k, cur := range e.running { // target may be unset on early failure
		if cur == r {
			delete(e.running, k)
		}
	}
	if keep {
		e.addHistoryLocked(r)
	}
	select {
	case <-r.done:
	default:
		close(r.done)
	}
}

func (e *Engine) record(r *run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addHistoryLocked(r)
}

func (e *Engine) addHistoryLocked(r *run) {
	for _, h := range e.history {
		if h == r {
			return
		}
	}
	e.history = append(e.history, r)
	for len(e.history) > e.opts.HistorySize {
		e.history = e.history[1:]
	}
}

func (e *Engine) find(id string) *run {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.history {
		if r.exp.ID == id {
			return r
		}
	}
	return nil
}

func (e *Engine) publish(r *run) {
	if e.pub != nil {
		e.pub.Publish(models.StreamExperiment, r.snapshot())
	}
}

func (r *run) snapshot() models.Experiment {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.exp
	x.Measurements = append([]models.Measurement(nil), r.exp.Measurements...)
	x.Timeline = append([]models.TimelineEntry(nil), r.exp.Timeline...)
	x.Caveats = append([]string(nil), r.exp.Caveats...)
	if x.Measurements == nil {
		x.Measurements = []models.Measurement{}
	}
	if x.Timeline == nil {
		x.Timeline = []models.TimelineEntry{}
	}
	if x.Caveats == nil {
		x.Caveats = []string{}
	}
	return x
}

// setStartLocked makes the request start the timeline's zero point and
// re-bases entries recorded before it (baseline, explanation), which then get
// a negative offset instead of a misleading positive one.
func (r *run) setStartLocked(t time.Time) {
	r.t0 = t
	for i := range r.exp.Timeline {
		r.exp.Timeline[i].OffsetMs = r.exp.Timeline[i].At.Sub(t).Milliseconds()
	}
}

func (r *run) addLocked(kind string, observed bool, resource, msg string) {
	r.addAtLocked(r.e.opts.Now(), kind, observed, resource, msg)
}

func (r *run) addAtLocked(at time.Time, kind string, observed bool, resource, msg string) {
	if len(r.exp.Timeline) >= r.e.opts.MaxTimeline {
		r.exp.TimelineTruncated = true
		return
	}
	r.exp.Timeline = append(r.exp.Timeline, models.TimelineEntry{
		At: at, OffsetMs: at.Sub(r.t0).Milliseconds(), Kind: kind, Observed: observed, Resource: resource, Message: msg,
	})
}

func (r *run) finishLocked(status, reason string) {
	if r.exp.Status != models.StatusRunning {
		return
	}
	now := r.e.opts.Now()
	r.exp.Status = status
	r.exp.StatusReason = reason
	r.exp.FinishedAt = &now
	r.addLocked(models.KindStatus, false, "", fmt.Sprintf("Experiment %s: %s", status, reason))
	if r.droppedObs > 0 {
		r.exp.Caveats = append(r.exp.Caveats, fmt.Sprintf("%d watch observations were dropped because the experiment queue was full; polling was used as a fallback.", r.droppedObs))
	}
	r.computeLocked()
}

// loop drives one experiment until success, timeout, interruption or failure.
func (r *run) loop(initial *appsv1.Deployment) {
	e := r.e
	defer e.release(r, true)
	defer e.publish(r)
	timeout := time.NewTimer(e.opts.Timeout)
	defer timeout.Stop()
	poll := time.NewTicker(e.opts.PollInterval)
	defer poll.Stop()

	if initial != nil {
		r.mu.Lock()
		r.onDeploymentLocked(initial, e.opts.Now(), "api_response")
		done := r.exp.Status != models.StatusRunning
		r.mu.Unlock()
		if done {
			return
		}
	}
	r.pollOnce()
	for {
		r.mu.Lock()
		done := r.exp.Status != models.StatusRunning
		r.mu.Unlock()
		if done {
			return
		}
		select {
		case <-r.ctx.Done():
			r.mu.Lock()
			r.finishLocked(models.StatusInterrupted, context.Cause(r.ctx).Error())
			r.mu.Unlock()
			return
		case <-timeout.C:
			r.mu.Lock()
			reason := fmt.Sprintf("not converged within %s", e.opts.Timeout)
			if r.lastDeployment != nil {
				reason += ": " + kube.DeploymentConverged(r.lastDeployment, r.desired).Reason
			}
			if r.lastPollErr != "" {
				reason += " (last API error: " + r.lastPollErr + ")"
			}
			r.finishLocked(models.StatusTimeout, reason)
			r.mu.Unlock()
			return
		case <-poll.C:
			r.pollOnce()
		case o := <-r.obsCh:
			r.mu.Lock()
			changed := r.onObservationLocked(o)
			r.mu.Unlock()
			if changed {
				e.publish(r)
			}
		}
	}
}

// pollOnce is a fallback in case watch events are delayed or lost.
func (r *run) pollOnce() {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	d, err := r.e.ops.GetDeployment(ctx)
	now := r.e.opts.Now()
	r.mu.Lock()
	if err != nil {
		if r.ctx.Err() == nil && r.lastPollErr != err.Error() {
			r.addAtLocked(now, models.KindPoll, true, "Deployment/"+r.e.opts.Policy.Deployment, "Poll failed: "+err.Error())
		}
		r.lastPollErr = err.Error()
		r.mu.Unlock()
		return
	}
	r.lastPollErr = ""
	r.onDeploymentLocked(d, now, "poll")
	var checkPod bool
	if r.exp.Type == models.TypeDeletePod && r.goneAt == nil {
		checkPod = true
	}
	r.mu.Unlock()
	if checkPod {
		p, err := r.e.ops.GetPod(ctx, r.exp.Target.PodName)
		now := r.e.opts.Now()
		r.mu.Lock()
		if (err != nil && isNotFound(err)) || (err == nil && p.UID != r.podUID) {
			if r.goneAt == nil {
				r.goneAt = &now
				r.addAtLocked(now, models.KindPoll, true, "Pod/"+r.exp.Target.PodName, "Original Pod (uid "+string(r.podUID)+") is absent from the API")
			}
		}
		r.mu.Unlock()
	}
	r.e.publish(r)
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func (r *run) onObservationLocked(o kube.Observation) bool {
	switch obj := o.Obj.(type) {
	case *appsv1.Deployment:
		if obj.Name != r.e.opts.Policy.Deployment {
			return false
		}
		r.onDeploymentLocked(obj, o.At, "watch")
		return true
	case *appsv1.ReplicaSet:
		desired := int32(0)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		r.addAtLocked(o.At, models.KindWatch, true, "ReplicaSet/"+obj.Name,
			fmt.Sprintf("%s: spec.replicas=%d status.replicas=%d ready=%d available=%d", o.Action, desired, obj.Status.Replicas, obj.Status.ReadyReplicas, obj.Status.AvailableReplicas))
		return true
	case *corev1.Pod:
		return r.onPodLocked(obj, o.Action, o.At)
	case *corev1.Event:
		if !r.relevantEvent(obj) {
			return false
		}
		ev := kube.EventToInfo(obj)
		r.addAtLocked(o.At, models.KindK8sEvent, true, obj.InvolvedObject.Kind+"/"+obj.InvolvedObject.Name,
			fmt.Sprintf("%s %s (%s): %s", ev.Type, ev.Reason, ev.Source, ev.Message))
		return true
	}
	return false
}

func (r *run) relevantEvent(ev *corev1.Event) bool {
	if r.exp.RequestStartedAt == nil {
		return false
	}
	t := kube.EventToInfo(ev).LastObservedAt
	// Events carry the cluster's clock; allow modest skew but skip old history.
	if t.Before(r.exp.RequestStartedAt.Add(-5 * time.Second)) {
		return false
	}
	name := ev.InvolvedObject.Name
	dep := r.e.opts.Policy.Deployment
	return name == dep || (len(name) > len(dep) && name[:len(dep)+1] == dep+"-")
}

func (r *run) onDeploymentLocked(d *appsv1.Deployment, at time.Time, source string) {
	prev := r.lastDeployment
	r.lastDeployment = d
	kind := models.KindWatch
	if source == "poll" {
		kind = models.KindPoll
	} else if source == "api_response" {
		kind = models.KindAPIResponse
	}
	if prev == nil || prev.ResourceVersion != d.ResourceVersion {
		if source != "api_response" {
			r.addAtLocked(at, kind, true, "Deployment/"+d.Name, kube.DeploymentSummary(d))
		}
	}
	spec := kube.SpecReplicas(d)
	if spec != r.desired {
		r.finishLocked(models.StatusInterrupted, fmt.Sprintf("spec.replicas changed to %d by another actor (expected %d)", spec, r.desired))
		return
	}
	if d.Generation > r.targetGen && r.exp.Type == models.TypeScale && !r.hasCaveat("generation") {
		r.exp.Caveats = append(r.exp.Caveats, fmt.Sprintf("Deployment generation advanced to %d after KubeScope's PATCH (generation %d); another change (e.g. template) happened during the experiment.", d.Generation, r.targetGen))
	}
	ready := d.Status.ReadyReplicas
	if r.minReady == nil || ready < *r.minReady {
		v := ready
		r.minReady = &v
		r.exp.Target.MinReadyObserved = &v
	}
	if r.exp.Type == models.TypeDeletePod && r.dipAt == nil && ready < r.exp.Target.BaselineReady {
		t := at
		r.dipAt = &t
		r.addAtLocked(at, models.KindObservation, true, "Deployment/"+d.Name, fmt.Sprintf("Readiness dip observed: ready=%d < baseline %d", ready, r.exp.Target.BaselineReady))
	}
	r.evaluateLocked(at)
}

func (r *run) hasCaveat(sub string) bool {
	for _, c := range r.exp.Caveats {
		if len(c) >= len(sub) && contains(c, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func (r *run) onPodLocked(p *corev1.Pod, action string, at time.Time) bool {
	if r.podStates == nil {
		r.podStates = map[types.UID]string{}
	}
	ready, _ := kube.PodReady(p)
	state := fmt.Sprintf("%s|%s|%t|%t", p.Status.Phase, p.Spec.NodeName, ready, p.DeletionTimestamp != nil)
	if action == "DELETED" {
		state = "deleted"
	}
	if r.podStates[p.UID] == state {
		return false
	}
	r.podStates[p.UID] = state
	msg := action + ": " + kube.PodSummary(p)
	if action == "DELETED" {
		msg = "DELETED (object removed from the API)"
	}
	r.addAtLocked(at, models.KindWatch, true, "Pod/"+p.Name, msg)

	if r.exp.Type != models.TypeDeletePod {
		return true
	}
	if p.UID == r.podUID {
		if p.DeletionTimestamp != nil && r.terminatingAt == nil {
			t := at
			r.terminatingAt = &t
		}
		if action == "DELETED" && r.goneAt == nil {
			t := at
			r.goneAt = &t
		}
	} else if !r.baselineUIDs[p.UID] && action != "DELETED" && isDemoPod(p, r.e.opts.Policy.Deployment) {
		if r.replacementUID == "" {
			r.replacementUID = p.UID
			t := at
			r.replacementAt = &t
			r.exp.Target.ReplacementPod = p.Name
			r.exp.Target.ReplacementUID = string(p.UID)
			r.addAtLocked(at, models.KindObservation, true, "Pod/"+p.Name, fmt.Sprintf("Replacement Pod observed with new UID %s (original %s)", p.UID, r.podUID))
		}
		if p.UID == r.replacementUID && ready && r.replacementRdy == nil {
			t := at
			r.replacementRdy = &t
			r.addAtLocked(at, models.KindObservation, true, "Pod/"+p.Name, "Replacement Pod reports Ready (readiness probe passing)")
		}
	}
	r.evaluateLocked(at)
	return true
}

func isDemoPod(p *corev1.Pod, deployment string) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Kind == "ReplicaSet" && len(ref.Name) > len(deployment) && ref.Name[:len(deployment)+1] == deployment+"-" {
			return true
		}
	}
	return false
}

// evaluateLocked checks success criteria.
func (r *run) evaluateLocked(at time.Time) {
	if r.exp.Status != models.StatusRunning || r.lastDeployment == nil || r.exp.RequestAcknowledgedAt == nil {
		return
	}
	c := kube.DeploymentConverged(r.lastDeployment, r.desired)
	switch r.exp.Type {
	case models.TypeScale:
		if c.Converged && r.lastDeployment.Status.ObservedGeneration >= r.targetGen {
			t := at
			r.exp.ConvergedAt = &t
			r.addAtLocked(at, models.KindObservation, true, "Deployment/"+r.lastDeployment.Name, "Converged: "+c.Reason)
			r.finishLocked(models.StatusSuccess, "Deployment converged to "+fmt.Sprint(r.desired)+" ready/available replicas")
		}
	case models.TypeDeletePod:
		if c.Converged && r.replacementRdy != nil {
			t := at
			r.exp.ConvergedAt = &t
			r.addAtLocked(at, models.KindObservation, true, "Deployment/"+r.lastDeployment.Name, "Desired readiness restored: "+c.Reason)
			reason := "replacement Pod Ready and Deployment back to desired readiness"
			if r.dipAt == nil {
				reason += " (no readiness dip was observed)"
			}
			r.finishLocked(models.StatusSuccess, reason)
		}
	}
}

// computeLocked rebuilds the measurement list from recorded timestamps.
func (r *run) computeLocked() {
	x := &r.exp
	var ms []models.Measurement
	notDone := "experiment ended with status " + x.Status + " before this point was observed"
	if x.Status == models.StatusRunning {
		notDone = "not observed yet"
	}
	verb := "PATCH"
	if x.Type == models.TypeDeletePod {
		verb = "DELETE"
	}
	ms = append(ms, span("api_rtt", "API request round-trip", x.RequestStartedAt, x.RequestAcknowledgedAt,
		"KubeScope sends the "+verb+" request", "KubeScope receives the API server's success response",
		"Client-observed latency of a single API call (network + authn/authz + admission + etcd write). It says nothing about workload readiness.",
		"request did not complete"))
	switch x.Type {
	case models.TypeScale:
		ms = append(ms, span("time_to_convergence", "Acknowledged → converged", x.RequestAcknowledgedAt, x.ConvergedAt,
			"PATCH acknowledged by the API server",
			"First observation with observedGeneration ≥ patched generation and updated = ready = available = status.replicas = desired",
			"Includes controller reconciliation, scheduling, image pulls, container start, readiness probes and watch/poll delivery to KubeScope. Not a pure control-plane latency.",
			notDone))
		ms = append(ms, span("end_to_end", "Request start → converged", x.RequestStartedAt, x.ConvergedAt,
			"KubeScope sends the PATCH request", "Convergence observed",
			"api_rtt + time_to_convergence.", notDone))
	case models.TypeDeletePod:
		ms = append(ms, span("time_to_replacement_observed", "Acknowledged → replacement Pod seen", x.RequestAcknowledgedAt, r.replacementAt,
			"DELETE acknowledged", "First watch observation of a Pod with a UID not present before deletion",
			"How long until the ReplicaSet controller's new Pod object was visible to KubeScope (not yet scheduled or running).",
			notDone))
		ms = append(ms, span("time_to_replacement_ready", "Acknowledged → replacement Ready", x.RequestAcknowledgedAt, r.replacementRdy,
			"DELETE acknowledged", "Replacement Pod's Ready condition observed True",
			"Includes scheduling, image pull (often cached), container start and readiness probe period.", notDone))
		ms = append(ms, span("time_to_desired_readiness", "Acknowledged → desired readiness restored", x.RequestAcknowledgedAt, x.ConvergedAt,
			"DELETE acknowledged", fmt.Sprintf("Deployment converged at baseline desired=%d with replacement Ready", x.Target.BaselineDesired),
			"End of the recovery as seen through Deployment status.", notDone))
		dip := span("recovery_duration", "Readiness dip → restored", r.dipAt, x.ConvergedAt,
			fmt.Sprintf("First observation of readyReplicas below the baseline (%d)", x.Target.BaselineReady), "Desired readiness restored",
			"Duration ready capacity was observed below baseline. Only measured if a dip was actually observed.", notDone)
		if r.dipAt == nil {
			if x.Status == models.StatusRunning {
				dip.Reason = "no readiness dip observed yet"
			} else {
				dip.Reason = fmt.Sprintf("No readiness dip was observed: ready replicas never fell below the baseline of %d in any watch or poll observation. A dip shorter than the observation granularity can be missed; no recovery interval is reported.", x.Target.BaselineReady)
			}
		}
		ms = append(ms, dip)
		ms = append(ms, span("time_to_original_absent", "Acknowledged → original Pod gone", x.RequestAcknowledgedAt, r.goneAt,
			"DELETE acknowledged", "Original Pod UID no longer present in the API (watch DELETED or poll 404)",
			"Includes the graceful termination period. Recovery does not wait for this.",
			func() string {
				if x.Status == models.StatusRunning {
					return "not observed yet"
				}
				return "original Pod was still terminating (graceful shutdown) when the experiment finished"
			}()))
	}
	x.Measurements = ms
}

func span(name, label string, start, end *time.Time, startCond, endCond, desc, missing string) models.Measurement {
	m := models.Measurement{Name: name, Label: label, Start: startCond, End: endCond, Description: desc, StartAt: start, EndAt: end}
	if start != nil && end != nil {
		v := end.Sub(*start).Milliseconds()
		if v < 0 {
			v = 0
		}
		m.ValueMs = &v
	} else {
		m.Reason = missing
	}
	return m
}

func scaleCaveats(gke bool) []string {
	c := []string{
		"Timestamps are taken on the KubeScope host when requests return and when watch/poll observations arrive, not inside the control plane.",
		"Time-to-convergence includes scheduling, image pulls, container start-up and readiness probes; it is not control-plane latency.",
		"Polling every 2s backs up the watch, so convergence can be detected up to one watch-delivery or poll interval late.",
	}
	if gke {
		c = append(c, "GKE: the control plane is managed by Google; KubeScope only sees the public Kubernetes API and cannot measure internal control-plane latency.")
	}
	return c
}

func deleteCaveats(gke bool) []string {
	c := []string{
		"Timestamps are taken on the KubeScope host when requests return and when watch/poll observations arrive, not inside the control plane.",
		"The replacement is identified as the first demo Pod whose UID was not present before deletion. Another actor creating Pods at the same time could confuse this.",
		"A readiness dip shorter than the watch delivery or poll interval may not be observed.",
	}
	if gke {
		c = append(c, "GKE: the control plane is managed by Google; KubeScope only sees the public Kubernetes API.")
	}
	return c
}

// SortSummaries is a helper for deterministic output in tests.
func SortSummaries(s []models.ExperimentSummary) {
	sort.Slice(s, func(i, j int) bool { return s[i].CreatedAt.After(s[j].CreatedAt) })
}
