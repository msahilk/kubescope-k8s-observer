package kube

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func dep(spec int32, gen, obsGen int64, cur, upd, ready, avail int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Generation: gen},
		Spec:       appsv1.DeploymentSpec{Replicas: &spec},
		Status: appsv1.DeploymentStatus{ObservedGeneration: obsGen, Replicas: cur, UpdatedReplicas: upd,
			ReadyReplicas: ready, AvailableReplicas: avail},
	}
}

func TestDeploymentConverged(t *testing.T) {
	cases := []struct {
		name string
		d    *appsv1.Deployment
		want int32
		ok   bool
	}{
		{"fully converged", dep(4, 2, 2, 4, 4, 4, 4), 4, true},
		{"nil deployment", nil, 4, false},
		{"spec differs from desired (external change)", dep(3, 2, 2, 3, 3, 3, 3), 4, false},
		{"observedGeneration lags: status is about the old spec", dep(4, 3, 2, 4, 4, 4, 4), 4, false},
		{"updated replicas lag (rollout)", dep(4, 2, 2, 4, 3, 4, 4), 4, false},
		{"ready lags", dep(4, 2, 2, 4, 4, 3, 3), 4, false},
		{"available lags (minReadySeconds)", dep(4, 2, 2, 4, 4, 4, 3), 4, false},
		{"surplus pods during scale-down", dep(2, 2, 2, 3, 2, 2, 2), 2, false},
		{"spec.replicas alone is not success", dep(4, 2, 2, 2, 2, 2, 2), 4, false},
		{"observedGeneration ahead is fine", dep(2, 2, 3, 2, 2, 2, 2), 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeploymentConverged(c.d, c.want)
			if got.Converged != c.ok {
				t.Fatalf("Converged=%v want %v (reason %q)", got.Converged, c.ok, got.Reason)
			}
			if got.Reason == "" {
				t.Fatal("reason must always be set")
			}
		})
	}
}

func TestSpecReplicasDefault(t *testing.T) {
	d := &appsv1.Deployment{}
	if SpecReplicas(d) != 1 {
		t.Fatal("nil spec.replicas defaults to 1")
	}
}

func TestPodRunningVsReady(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}}
	info := PodToInfo(p)
	if !info.Running || info.Ready {
		t.Fatalf("running-but-not-ready pod reported running=%v ready=%v", info.Running, info.Ready)
	}
	p.Status.Conditions[0].Status = corev1.ConditionTrue
	if !PodToInfo(p).Ready {
		t.Fatal("ready condition not detected")
	}
	if PodToInfo(&corev1.Pod{}).Phase != "Unknown" {
		t.Fatal("empty phase should be Unknown")
	}
}

func TestEventTimestampFallbacks(t *testing.T) {
	now := metav1.Now()
	e := &corev1.Event{EventTime: metav1.NewMicroTime(now.Time), ReportingController: "deployment-controller"}
	info := EventToInfo(e)
	if !info.LastObservedAt.Equal(now.Time) || info.Source != "deployment-controller" {
		t.Fatalf("unexpected %+v", info)
	}
}
