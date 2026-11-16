package kube

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sahilkamil/kubescope/backend/internal/config"
)

type recorder struct {
	mu   sync.Mutex
	seen map[string]int
}

func (r *recorder) Observe(o Observation) {
	m, _ := metaOf(o.Obj)
	r.mu.Lock()
	r.seen[o.Kind+"/"+o.Action+"/"+m.GetName()]++
	r.mu.Unlock()
}

func (r *recorder) count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[key]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func labeledPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kubescope-demo", UID: types.UID("uid-" + name),
		ResourceVersion: "1", Labels: map[string]string{config.PartOfLabelKey: config.PartOfLabelValue}}}
}

// Restart replaces the informers (fresh LIST + WATCH); unchanged objects are
// not re-emitted, and changes made afterwards are still delivered.
func TestWatcherRestart(t *testing.T) {
	cs := fake.NewSimpleClientset(labeledPod("a"))
	cfg, err := config.LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c := NewWithClientset(cs, Identity{Context: "test"}, cfg)
	rec := &recorder{seen: map[string]int{}}
	w := NewWatcher(c, rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	waitFor(t, "initial sync", func() bool { return w.Status().Synced && rec.count("Pod/ADDED/a") == 1 })
	w.Restart()
	waitFor(t, "restart", func() bool { return w.Status().Restarts == 1 && w.Status().Synced })
	if n := rec.count("Pod/ADDED/a"); n != 1 {
		t.Errorf("unchanged Pod re-emitted after restart: ADDED count = %d", n)
	}
	if _, err := cs.CoreV1().Pods("kubescope-demo").Create(context.Background(), labeledPod("b"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Pod b after restart", func() bool { return rec.count("Pod/ADDED/b") == 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
