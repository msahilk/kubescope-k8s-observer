package safety

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var pol = Policy{
	AllowedNamespaces: []string{"kubescope-demo"}, Namespace: "kubescope-demo", Deployment: "demo-api",
	LabelKey: "app.kubernetes.io/part-of", LabelValue: "kubescope-demo", MinReplicas: 1, MaxReplicas: 5,
}

var demoLabels = map[string]string{"app.kubernetes.io/part-of": "kubescope-demo"}

func TestNamespace(t *testing.T) {
	if err := pol.CheckNamespace("kubescope-demo"); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"default", "kube-system", ""} {
		if err := pol.CheckNamespace(ns); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%q: want forbidden, got %v", ns, err)
		}
	}
	p := pol
	p.AllowedNamespaces = []string{"other"}
	if err := p.CheckNamespace("kubescope-demo"); !errors.Is(err, ErrForbidden) {
		t.Fatal("namespace not on allowlist must be forbidden")
	}
}

func TestReplicasAndNames(t *testing.T) {
	for _, n := range []int32{1, 3, 5} {
		if err := pol.ValidateReplicas(n); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int32{-1, 0, 6, 100} {
		if err := pol.ValidateReplicas(n); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%d should be invalid", n)
		}
	}
	if err := pol.ValidateDeploymentName("demo-api"); err != nil {
		t.Fatal(err)
	}
	if err := pol.ValidateDeploymentName("coredns"); !errors.Is(err, ErrForbidden) {
		t.Fatal("other deployment must be forbidden")
	}
	if err := pol.ValidateDeploymentName("Bad_Name"); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid name must be invalid")
	}
	for _, n := range []string{"", "x;rm -rf /", "../etc", "UPPER"} {
		if err := ValidatePodName(n); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q should be invalid", n)
		}
	}
}

func ctrl(kind, name string) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &t}}
}

func TestPodDeletable(t *testing.T) {
	owner := func(rs string) (string, error) {
		if rs == "demo-api-abc" {
			return "demo-api", nil
		}
		return "something-else", nil
	}
	good := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-abc-1", Namespace: "kubescope-demo", Labels: demoLabels, OwnerReferences: ctrl("ReplicaSet", "demo-api-abc")}}
	if err := pol.CheckPodDeletable(good, owner); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(p *corev1.Pod){
		"other namespace":  func(p *corev1.Pod) { p.Namespace = "kube-system" },
		"missing label":    func(p *corev1.Pod) { p.Labels = nil },
		"no controller":    func(p *corev1.Pod) { p.OwnerReferences = nil },
		"owned by a Job":   func(p *corev1.Pod) { p.OwnerReferences = ctrl("Job", "x") },
		"other deployment": func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet", "other-rs") },
		"already deleting": func(p *corev1.Pod) { n := metav1.Now(); p.DeletionTimestamp = &n },
	}
	for name, mut := range cases {
		p := good.DeepCopy()
		mut(p)
		if err := pol.CheckPodDeletable(p, owner); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}
