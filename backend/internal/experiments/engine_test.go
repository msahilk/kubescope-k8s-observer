package experiments

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sahilkamil/kubescope/backend/internal/kube"
	"github.com/sahilkamil/kubescope/backend/internal/models"
	"github.com/sahilkamil/kubescope/backend/internal/safety"
)

var labels = map[string]string{"app.kubernetes.io/part-of": "kubescope-demo"}

var policy = safety.Policy{AllowedNamespaces: []string{"kubescope-demo"}, Namespace: "kubescope-demo", Deployment: "demo-api",
	LabelKey: "app.kubernetes.io/part-of", LabelValue: "kubescope-demo", MinReplicas: 1, MaxReplicas: 5}

// fakeOps is a scripted stand-in for the Kubernetes API.
type fakeOps struct {
	mu       sync.Mutex
	d        *appsv1.Deployment
	pods     map[string]*corev1.Pod
	patchErr error
	getErr   error
	patches  int
}

func newOps(replicas int32) *fakeOps {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "kubescope-demo", Labels: labels, Generation: 1, ResourceVersion: "1"},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas}}
	o := &fakeOps{d: d, pods: map[string]*corev1.Pod{}}
	for _, n := range []string{"demo-api-rs-a", "demo-api-rs-b"}[:replicas] {
		o.pods[n] = pod(n, types.UID("uid-"+n), true)
	}
	return o
}

func pod(name string, uid types.UID, ready bool) *corev1.Pod {
	t := true
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Namespace: "kubescope-demo", Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "demo-api-rs", Controller: &t}}},
		Spec:   corev1.PodSpec{NodeName: "n1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}

func (f *fakeOps) GetDeployment(context.Context) (*appsv1.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.d.DeepCopy(), nil
}
func (f *fakeOps) PatchReplicas(_ context.Context, n int32) (*appsv1.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches++
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	if *f.d.Spec.Replicas != n {
		f.d.Spec.Replicas = &n
		f.d.Generation++
	}
	return f.d.DeepCopy(), nil
}
func (f *fakeOps) GetPod(_ context.Context, name string) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pods[name]; ok {
		return p.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}
func (f *fakeOps) ListPods(context.Context) ([]corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.Pod
	for _, p := range f.pods {
		out = append(out, *p.DeepCopy())
	}
	return out, nil
}
func (f *fakeOps) ReplicaSetOwner(_ context.Context, rs string) (string, error) {
	if rs == "demo-api-rs" {
		return "demo-api", nil
	}
	return "", nil
}
func (f *fakeOps) DeletePod(_ context.Context, name string, uid types.UID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[name]
	if !ok || p.UID != uid {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, name, errors.New("uid precondition failed"))
	}
	delete(f.pods, name)
	return nil
}

// setStatus mutates the fake deployment and returns an observation of it.
func (f *fakeOps) setStatus(obsGen int64, cur, upd, ready, avail int32) kube.Observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.d.Status = appsv1.DeploymentStatus{ObservedGeneration: obsGen, Replicas: cur, UpdatedReplicas: upd, ReadyReplicas: ready, AvailableReplicas: avail}
	f.d.ResourceVersion += "x"
	return kube.Observation{Kind: "Deployment", Action: "MODIFIED", Obj: f.d.DeepCopy(), At: time.Now()}
}

type nopPub struct{}

func (nopPub) Publish(string, any) models.StreamEvent { return models.StreamEvent{} }

func newEngine(ops Ops, timeout time.Duration) *Engine {
	return New(ops, nopPub{}, Options{Timeout: timeout, PollInterval: time.Hour, Policy: policy, Identity: kube.Identity{Context: "kind-kubescope", Cluster: "kind-kubescope"}})
}

func waitStatus(t *testing.T, e *Engine, id string) models.Experiment {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		x, err := e.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if x.Status != models.StatusRunning {
			return x
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("experiment did not finish")
	return models.Experiment{}
}

func measurement(x models.Experiment, name string) models.Measurement {
	for _, m := range x.Measurements {
		if m.Name == name {
			return m
		}
	}
	return models.Measurement{}
}

func TestScaleSuccessRequiresObservedGeneration(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, err := e.StartScale(context.Background(), "demo-api", 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if x.RequestStartedAt == nil || x.RequestAcknowledgedAt == nil || x.ConvergedAt != nil {
		t.Fatalf("timestamps wrong after start: %+v", x)
	}
	if m := measurement(x, "api_rtt"); m.ValueMs == nil {
		t.Fatal("api_rtt must be measured once acknowledged")
	}
	// Status claims 4 ready, but for the OLD generation: must not be success.
	e.Observe(ops.setStatus(1, 4, 4, 4, 4))
	time.Sleep(30 * time.Millisecond)
	if cur, _ := e.Get(x.ID); cur.Status != models.StatusRunning {
		t.Fatalf("must not converge on stale observedGeneration, got %s", cur.Status)
	}
	e.Observe(ops.setStatus(2, 4, 4, 3, 3))
	e.Observe(ops.setStatus(2, 4, 4, 4, 4))
	done := waitStatus(t, e, x.ID)
	if done.Status != models.StatusSuccess || done.ConvergedAt == nil {
		t.Fatalf("want success, got %s (%s)", done.Status, done.StatusReason)
	}
	if m := measurement(done, "time_to_convergence"); m.ValueMs == nil || m.Start == "" || m.End == "" {
		t.Fatalf("convergence measurement incomplete: %+v", m)
	}
	if done.Target.FromReplicas != 2 || done.Target.ToReplicas != 4 {
		t.Fatalf("target %+v", done.Target)
	}
}

func TestScaleInterruptedByExternalChange(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, _ := e.StartScale(context.Background(), "demo-api", 4, false)
	ops.mu.Lock()
	three := int32(3)
	ops.d.Spec.Replicas = &three
	ops.d.Generation++
	ops.mu.Unlock()
	e.Observe(ops.setStatus(3, 3, 3, 3, 3))
	done := waitStatus(t, e, x.ID)
	if done.Status != models.StatusInterrupted {
		t.Fatalf("want interrupted, got %s", done.Status)
	}
	if m := measurement(done, "time_to_convergence"); m.ValueMs != nil || m.Reason == "" {
		t.Fatal("no convergence value may be reported for an interrupted run")
	}
}

func TestScaleTimeoutAndLockRelease(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 80*time.Millisecond)
	x, _ := e.StartScale(context.Background(), "demo-api", 3, false)
	if _, err := e.StartScale(context.Background(), "demo-api", 4, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("second concurrent experiment must conflict, got %v", err)
	}
	if _, err := e.StartDeletePod(context.Background(), "demo-api-rs-a", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete-pod during scale must conflict, got %v", err)
	}
	done := waitStatus(t, e, x.ID)
	if done.Status != models.StatusTimeout {
		t.Fatalf("want timeout, got %s", done.Status)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := e.StartScale(context.Background(), "demo-api", 2, false); err != nil {
		t.Fatalf("lock must be released after timeout: %v", err)
	}
}

func TestCancel(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, _ := e.StartScale(context.Background(), "demo-api", 3, false)
	done, err := e.Cancel(x.ID)
	if err != nil || done.Status != models.StatusInterrupted {
		t.Fatalf("cancel: %v %s", err, done.Status)
	}
	if _, err := e.Cancel(x.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatal("cancelling a finished run must report not running")
	}
	if _, err := e.Cancel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown id")
	}
}

func TestPatchFailureIsRecordedAndUnlocks(t *testing.T) {
	ops := newOps(2)
	ops.patchErr = apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "demo-api", errors.New("rbac"))
	e := newEngine(ops, time.Second)
	x, err := e.StartScale(context.Background(), "demo-api", 3, false)
	if !apierrors.IsForbidden(err) || x.Status != models.StatusFailed {
		t.Fatalf("err=%v status=%s", err, x.Status)
	}
	ops.patchErr = nil
	if _, err := e.StartScale(context.Background(), "demo-api", 3, false); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	if len(e.List()) != 2 {
		t.Fatalf("history should hold both runs, got %d", len(e.List()))
	}
}

func TestValidationRejectsBeforeMutation(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, time.Second)
	for _, n := range []int32{0, 6, 50} {
		if _, err := e.StartScale(context.Background(), "demo-api", n, false); !errors.Is(err, safety.ErrInvalid) {
			t.Fatalf("%d: %v", n, err)
		}
	}
	if _, err := e.StartScale(context.Background(), "coredns", 2, false); !errors.Is(err, safety.ErrForbidden) {
		t.Fatal(err)
	}
	ops.mu.Lock()
	ops.d.Labels = nil
	ops.mu.Unlock()
	if _, err := e.StartScale(context.Background(), "demo-api", 3, false); !errors.Is(err, safety.ErrForbidden) {
		t.Fatalf("unlabeled deployment must be refused: %v", err)
	}
	if ops.patches != 0 || len(e.List()) != 0 {
		t.Fatal("no PATCH and no history entry for rejected requests")
	}
	if _, err := e.StartDeletePod(context.Background(), "demo-api-rs-a", false); !errors.Is(err, safety.ErrInvalid) {
		t.Fatal("confirm is required")
	}
}

func podObs(p *corev1.Pod, action string) kube.Observation {
	return kube.Observation{Kind: "Pod", Action: action, Obj: p, At: time.Now()}
}

func TestDeletePodReplacementAndDip(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, err := e.StartDeletePod(context.Background(), "demo-api-rs-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if x.Target.PodUID != "uid-demo-api-rs-a" || x.Target.BaselineReady != 2 {
		t.Fatalf("baseline %+v", x.Target)
	}
	orig := pod("demo-api-rs-a", "uid-demo-api-rs-a", true)
	now := metav1.Now()
	orig.DeletionTimestamp = &now
	e.Observe(podObs(orig, "MODIFIED"))
	// An object with the ORIGINAL uid re-observed must never count as replacement.
	e.Observe(podObs(pod("demo-api-rs-a", "uid-demo-api-rs-a", false), "MODIFIED"))
	e.Observe(ops.setStatus(1, 2, 2, 1, 1)) // dip
	repl := pod("demo-api-rs-c", "uid-new", false)
	e.Observe(podObs(repl, "ADDED"))
	e.Observe(podObs(orig, "DELETED"))
	time.Sleep(20 * time.Millisecond)
	if cur, _ := e.Get(x.ID); cur.Status != models.StatusRunning {
		t.Fatal("replacement not ready yet: must still be running")
	}
	e.Observe(podObs(pod("demo-api-rs-c", "uid-new", true), "MODIFIED"))
	e.Observe(ops.setStatus(1, 2, 2, 2, 2))
	done := waitStatus(t, e, x.ID)
	if done.Status != models.StatusSuccess {
		t.Fatalf("got %s: %s", done.Status, done.StatusReason)
	}
	if done.Target.ReplacementUID != "uid-new" || done.Target.ReplacementUID == done.Target.PodUID {
		t.Fatalf("replacement uid %q", done.Target.ReplacementUID)
	}
	for _, n := range []string{"api_rtt", "time_to_replacement_observed", "time_to_replacement_ready", "time_to_desired_readiness", "recovery_duration", "time_to_original_absent"} {
		if measurement(done, n).ValueMs == nil {
			t.Fatalf("%s should be measured: %+v", n, measurement(done, n))
		}
	}
	if done.Target.MinReadyObserved == nil || *done.Target.MinReadyObserved != 1 {
		t.Fatal("min ready should be 1")
	}
}

func TestDeletePodWithoutObservedDipReportsNoRecoveryInterval(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, _ := e.StartDeletePod(context.Background(), "demo-api-rs-b", true)
	e.Observe(podObs(pod("demo-api-rs-d", "uid-new2", true), "ADDED"))
	e.Observe(ops.setStatus(1, 2, 2, 2, 2))
	done := waitStatus(t, e, x.ID)
	if done.Status != models.StatusSuccess {
		t.Fatalf("got %s", done.Status)
	}
	m := measurement(done, "recovery_duration")
	if m.ValueMs != nil || m.Reason == "" {
		t.Fatalf("recovery must be unavailable with a reason when no dip was seen: %+v", m)
	}
}

func TestDeletePodRefusesForeignPods(t *testing.T) {
	ops := newOps(2)
	t1 := true
	ops.pods["intruder"] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "intruder", UID: "u9", Namespace: "kubescope-demo", Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "other-rs", Controller: &t1}}}}
	e := newEngine(ops, time.Second)
	if _, err := e.StartDeletePod(context.Background(), "intruder", true); !errors.Is(err, safety.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.StartDeletePod(context.Background(), "missing-pod", true); !apierrors.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	if _, ok := ops.pods["intruder"]; !ok {
		t.Fatal("foreign pod must not be deleted")
	}
}

func TestHistoryBounded(t *testing.T) {
	ops := newOps(2)
	ops.patchErr = errors.New("boom")
	e := New(ops, nopPub{}, Options{Timeout: time.Second, Policy: policy, HistorySize: 3})
	for i := 0; i < 5; i++ {
		_, _ = e.StartScale(context.Background(), "demo-api", 3, false)
	}
	if len(e.List()) != 3 {
		t.Fatalf("history size %d", len(e.List()))
	}
}

func TestCloseInterruptsRunning(t *testing.T) {
	ops := newOps(2)
	e := newEngine(ops, 5*time.Second)
	x, _ := e.StartScale(context.Background(), "demo-api", 3, false)
	e.Close()
	got, _ := e.Get(x.ID)
	if got.Status != models.StatusInterrupted {
		t.Fatalf("got %s", got.Status)
	}
}

// Timeline offsets are relative to the request start: entries recorded before
// the PATCH (the explanation) must not get a positive offset.
func TestTimelineOffsetsRelativeToRequestStart(t *testing.T) {
	ops := newOps(2)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var tick atomic.Int64
	e := New(ops, nopPub{}, Options{Timeout: time.Second, PollInterval: time.Hour, Policy: policy, Now: func() time.Time {
		return base.Add(time.Duration(tick.Add(1)) * 10 * time.Millisecond)
	}})
	x, err := e.StartScale(context.Background(), "demo-api", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = e.Cancel(x.ID) }()
	sawRequest := false
	for _, entry := range x.Timeline {
		switch entry.Kind {
		case models.KindInference:
			if !sawRequest && entry.OffsetMs > 0 {
				t.Errorf("pre-request entry has positive offset %d", entry.OffsetMs)
			}
		case models.KindAPIRequest:
			sawRequest = true
			if entry.OffsetMs != 0 {
				t.Errorf("API request offset = %d, want 0", entry.OffsetMs)
			}
		}
	}
	if !sawRequest {
		t.Fatal("no API request entry in timeline")
	}
}
