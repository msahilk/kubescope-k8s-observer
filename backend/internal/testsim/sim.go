// Package testsim is a TEST-ONLY, deliberately simplified imitation of the
// Deployment/ReplicaSet controllers, scheduler and kubelet, running against
// client-go's fake clientset. It exists so the experiment engine, watcher and
// HTTP API can be exercised end-to-end in `go test` without a cluster.
//
// It is NOT used by the real server, and nothing it produces should ever be
// presented as a measurement of a real Kubernetes cluster.
package testsim

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
)

// Labels applied to demo objects.
var Labels = map[string]string{"app": "demo-api", "app.kubernetes.io/part-of": "kubescope-demo"}

// Sim drives a fake clientset like a tiny cluster.
type Sim struct {
	CS        *fake.Clientset
	NS, Dep   string
	RSName    string
	Tick      time.Duration
	Reconcile time.Duration // delay before the "controller" observes a new generation
	Schedule  time.Duration // Pod created → bound to node
	Start     time.Duration // bound → Running
	Ready     time.Duration // Running → Ready

	mu       sync.Mutex
	lastSpec int32
	specAt   time.Time
	pods     map[types.UID]*podTimes
	rvs      int
}

type podTimes struct {
	created, scheduled, running time.Time
}

func ptr[T any](v T) *T { return &v }

// New seeds a namespace with a Ready Deployment of `replicas` Pods.
func New(ns, dep string, replicas int32) *Sim {
	s := &Sim{NS: ns, Dep: dep, RSName: dep + "-7d9f8c6b5", Tick: 20 * time.Millisecond,
		Reconcile: 30 * time.Millisecond, Schedule: 40 * time.Millisecond, Start: 40 * time.Millisecond, Ready: 60 * time.Millisecond,
		pods: map[types.UID]*podTimes{}, lastSpec: replicas}
	now := metav1.Now()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dep, Namespace: ns, UID: uuid.NewUUID(), Labels: Labels, Generation: 1, ResourceVersion: s.rv()},
		Spec: appsv1.DeploymentSpec{Replicas: ptr(replicas), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": dep}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27.3-alpine"}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: s.RSName, Namespace: ns, UID: uuid.NewUUID(), Labels: Labels, ResourceVersion: s.rv(),
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep, UID: d.UID, Controller: ptr(true)}}},
		Spec:   appsv1.ReplicaSetSpec{Replicas: ptr(replicas)},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
	objs := []runtime.Object{d, rs}
	for i := int32(0); i < replicas; i++ {
		p := s.newPod(rs, now.Time.Add(-time.Minute))
		p.Spec.NodeName = "sim-node"
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now}}
		objs = append(objs, p)
	}
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	s.CS = fake.NewSimpleClientset(objs...)
	return s
}

func (s *Sim) rv() string {
	s.rvs++
	return fmt.Sprint(1000 + s.rvs)
}

func (s *Sim) newPod(rs *appsv1.ReplicaSet, created time.Time) *corev1.Pod {
	uid := uuid.NewUUID()
	name := fmt.Sprintf("%s-%05d", rs.Name, rand.Intn(100000))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.NS, UID: uid, Labels: Labels, ResourceVersion: s.rv(),
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr(true)}}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27.3-alpine"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	s.pods[uid] = &podTimes{created: created}
	return p
}

// Run reconciles until ctx is done.
func (s *Sim) Run(ctx context.Context) {
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *Sim) reconcile(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	apps, core := s.CS.AppsV1(), s.CS.CoreV1()
	d, err := apps.Deployments(s.NS).Get(ctx, s.Dep, metav1.GetOptions{})
	if err != nil {
		return
	}
	desired := *d.Spec.Replicas
	// API-server behavior: a spec change bumps metadata.generation.
	if desired != s.lastSpec {
		s.lastSpec = desired
		s.specAt = now
		d.Generation++
		d.ResourceVersion = s.rv()
		if d, err = apps.Deployments(s.NS).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			return
		}
	}
	if d.Status.ObservedGeneration < d.Generation && now.Sub(s.specAt) < s.Reconcile {
		return // "controller" has not caught up yet
	}
	rs, err := apps.ReplicaSets(s.NS).Get(ctx, s.RSName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if *rs.Spec.Replicas != desired {
		rs.Spec.Replicas = ptr(desired)
		rs.ResourceVersion = s.rv()
		rs, _ = apps.ReplicaSets(s.NS).Update(ctx, rs, metav1.UpdateOptions{})
	}
	pl, err := core.Pods(s.NS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	var active []*corev1.Pod
	for i := range pl.Items {
		p := &pl.Items[i]
		if p.DeletionTimestamp == nil {
			active = append(active, p)
		}
	}
	for n := int32(len(active)); n < desired; n++ {
		p := s.newPod(rs, now)
		if p, err = core.Pods(s.NS).Create(ctx, p, metav1.CreateOptions{}); err == nil {
			active = append(active, p)
		}
	}
	for int32(len(active)) > desired {
		victim := active[len(active)-1]
		active = active[:len(active)-1]
		_ = core.Pods(s.NS).Delete(ctx, victim.Name, metav1.DeleteOptions{})
		delete(s.pods, victim.UID)
	}
	ready := int32(0)
	for _, p := range active {
		pt := s.pods[p.UID]
		if pt == nil {
			pt = &podTimes{created: p.CreationTimestamp.Time}
			s.pods[p.UID] = pt
		}
		changed := false
		if p.Spec.NodeName == "" && now.Sub(pt.created) >= s.Schedule {
			p.Spec.NodeName, pt.scheduled, changed = "sim-node", now, true
		}
		if p.Spec.NodeName != "" && p.Status.Phase == corev1.PodPending && !pt.scheduled.IsZero() && now.Sub(pt.scheduled) >= s.Start {
			p.Status.Phase, pt.running, changed = corev1.PodRunning, now, true
		}
		isReady := len(p.Status.Conditions) > 0 && p.Status.Conditions[0].Status == corev1.ConditionTrue
		if p.Status.Phase == corev1.PodRunning && !isReady && !pt.running.IsZero() && now.Sub(pt.running) >= s.Ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
			isReady, changed = true, true
		}
		if changed {
			p.ResourceVersion = s.rv()
			_, _ = core.Pods(s.NS).Update(ctx, p, metav1.UpdateOptions{})
		}
		if isReady {
			ready++
		}
	}
	n := int32(len(active))
	if rs.Status.Replicas != n || rs.Status.ReadyReplicas != ready {
		rs.Status = appsv1.ReplicaSetStatus{Replicas: n, ReadyReplicas: ready, AvailableReplicas: ready, ObservedGeneration: rs.Generation}
		rs.ResourceVersion = s.rv()
		_, _ = apps.ReplicaSets(s.NS).UpdateStatus(ctx, rs, metav1.UpdateOptions{})
	}
	st := appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: n, UpdatedReplicas: n, ReadyReplicas: ready, AvailableReplicas: ready, UnavailableReplicas: max(desired-ready, 0)}
	if d.Status.ObservedGeneration != st.ObservedGeneration || d.Status.Replicas != st.Replicas || d.Status.UpdatedReplicas != st.UpdatedReplicas ||
		d.Status.ReadyReplicas != st.ReadyReplicas || d.Status.AvailableReplicas != st.AvailableReplicas || d.Status.UnavailableReplicas != st.UnavailableReplicas {
		d.Status = st
		d.ResourceVersion = s.rv()
		_, _ = apps.Deployments(s.NS).UpdateStatus(ctx, d, metav1.UpdateOptions{})
	}
}
