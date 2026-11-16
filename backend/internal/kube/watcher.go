package kube

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// Observation is one change delivered by a watch (via an informer).
type Observation struct {
	Kind   string // Deployment | ReplicaSet | Pod | Event
	Action string // ADDED | MODIFIED | DELETED
	Obj    runtime.Object
	At     time.Time // when KubeScope received it (not when it happened in the cluster)
}

// Observer receives observations. Implementations must not block for long.
type Observer interface {
	Observe(o Observation)
}

// ObserverFunc adapts a function.
type ObserverFunc func(Observation)

// Observe implements Observer.
func (f ObserverFunc) Observe(o Observation) { f(o) }

// WatchStatus is exposed in /connection.
type WatchStatus struct {
	Synced      bool
	Errors      int
	Restarts    int // informer restarts after the API server became reachable again
	LastError   string
	LastErrorAt *time.Time
}

// Watcher runs shared informers for the demo namespace. Informers (reflectors)
// implement the resilient ListWatch pattern: initial LIST, WATCH from the
// returned resourceVersion, automatic re-LIST on "410 Gone"/expired
// resourceVersion, and exponential backoff on disconnects.
type Watcher struct {
	c   *Client
	obs []Observer

	mu     sync.Mutex
	status WatchStatus
	// lastRV de-duplicates repeated deliveries of the same object version
	// (e.g. after a relist), keyed by kind/uid.
	lastRV  map[string]string
	restart context.CancelFunc
}

// NewWatcher creates a watcher that fans observations out to observers.
func NewWatcher(c *Client, obs ...Observer) *Watcher {
	return &Watcher{c: c, obs: obs, lastRV: map[string]string{}}
}

// Status returns a copy of the watch status.
func (w *Watcher) Status() WatchStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Watcher) recordErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().UTC()
	w.status.Errors++
	w.status.LastError = err.Error()
	w.status.LastErrorAt = &now
}

// Run starts informers and blocks until ctx is done. A call to Restart stops
// the current informers and starts fresh ones (a full relist).
func (w *Watcher) Run(ctx context.Context) {
	if !w.c.Connected() {
		return
	}
	for {
		runCtx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		w.restart = cancel
		w.mu.Unlock()
		w.runOnce(runCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		w.status.Restarts++
		w.mu.Unlock()
	}
}

// Restart makes Run replace its informers with new ones, which LIST the
// current state and open new watches. It is called when the API server
// becomes reachable again after an outage: with some port forwards (kind
// behind Colima/Docker Desktop) a watch whose server went away can hang
// silently instead of failing, and the reflector would otherwise wait for its
// own timeouts and backoff (observed: about a minute) before relisting.
func (w *Watcher) Restart() {
	w.mu.Lock()
	cancel := w.restart
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *Watcher) runOnce(ctx context.Context) {
	ns := w.c.Policy.Namespace
	labeled := informers.NewSharedInformerFactoryWithOptions(w.c.CS, 0,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = w.c.LabelSelector() }))
	all := informers.NewSharedInformerFactoryWithOptions(w.c.CS, 0, informers.WithNamespace(ns))

	type entry struct {
		kind string
		inf  cache.SharedIndexInformer
	}
	entries := []entry{
		{"Deployment", labeled.Apps().V1().Deployments().Informer()},
		{"ReplicaSet", labeled.Apps().V1().ReplicaSets().Informer()},
		{"Pod", labeled.Core().V1().Pods().Informer()},
		{"Event", all.Core().V1().Events().Informer()},
	}
	var synced []cache.InformerSynced
	for _, e := range entries {
		kind := e.kind
		_ = e.inf.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
			w.recordErr(err)
			cache.DefaultWatchErrorHandler(r, err) // logs; reflector then relists/backs off
		})
		_, _ = e.inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { w.emit(kind, "ADDED", obj) },
			UpdateFunc: func(_, obj any) { w.emit(kind, "MODIFIED", obj) },
			DeleteFunc: func(obj any) {
				if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = t.Obj
				}
				w.emit(kind, "DELETED", obj)
			},
		})
		synced = append(synced, e.inf.HasSynced)
	}
	w.mu.Lock()
	w.status.Synced = false
	w.mu.Unlock()
	labeled.Start(ctx.Done())
	all.Start(ctx.Done())
	if cache.WaitForCacheSync(ctx.Done(), synced...) {
		w.mu.Lock()
		w.status.Synced = true
		w.mu.Unlock()
	}
	<-ctx.Done()
	labeled.Shutdown()
	all.Shutdown()
}

func (w *Watcher) emit(kind, action string, obj any) {
	ro, ok := obj.(runtime.Object)
	if !ok {
		return
	}
	m, err := metaOf(ro)
	if err != nil {
		return
	}
	key := kind + "/" + string(m.GetUID())
	w.mu.Lock()
	if action == "DELETED" {
		delete(w.lastRV, key)
	} else {
		if rv := m.GetResourceVersion(); rv != "" && w.lastRV[key] == rv {
			w.mu.Unlock()
			return // duplicate delivery of a version we already processed
		}
		w.lastRV[key] = m.GetResourceVersion()
	}
	w.mu.Unlock()
	o := Observation{Kind: kind, Action: action, Obj: ro, At: time.Now().UTC()}
	for _, ob := range w.obs {
		ob.Observe(o)
	}
}

func metaOf(o runtime.Object) (metav1.Object, error) {
	switch t := o.(type) {
	case *appsv1.Deployment:
		return t, nil
	case *appsv1.ReplicaSet:
		return t, nil
	case *corev1.Pod:
		return t, nil
	case *corev1.Event:
		return t, nil
	}
	return nil, errUnknownKind
}

type kindErr string

func (k kindErr) Error() string { return string(k) }

const errUnknownKind = kindErr("unknown object kind")
