package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/msahilk/kubescope-k8s-observer/backend/internal/config"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/experiments"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/kube"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/models"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/stream"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/testsim"
)

// stack wires the real server, engine, watcher and hub to a SIMULATED cluster
// (client-go fake clientset + testsim). This validates KubeScope's own logic
// and API contract; it is not evidence about real Kubernetes timing.
type stack struct {
	ts     *httptest.Server
	sim    *testsim.Sim
	engine *experiments.Engine
	cancel context.CancelFunc
}

func newStack(t *testing.T, mutate func(*config.Config)) *stack {
	t.Helper()
	cfg, err := config.LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExperimentTimeout = 10 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	sim := testsim.New(cfg.Namespace, cfg.Deployment, 2)
	client := kube.NewWithClientset(sim.CS, kube.Identity{Context: "sim", Cluster: "sim"}, cfg)
	hub := stream.NewHub(256)
	engine := experiments.New(client, hub, experiments.Options{Timeout: cfg.ExperimentTimeout, PollInterval: 100 * time.Millisecond, Identity: client.ID, Policy: kube.PolicyFrom(cfg)})
	monitor := kube.NewMonitor(client, time.Second, nil)
	watcher := kube.NewWatcher(client, engine, ResourcePublisher(hub, cfg.Deployment))
	ctx, cancel := context.WithCancel(context.Background())
	go sim.Run(ctx)
	go watcher.Run(ctx)
	monitor.Check(ctx)
	srv := &Server{Cfg: cfg, Client: client, Engine: engine, Hub: hub, Monitor: monitor, Watcher: watcher, HeartbeatEvery: 50 * time.Millisecond}
	ts := httptest.NewServer(srv.Handler())
	deadline := time.Now().Add(3 * time.Second)
	for !watcher.Status().Synced && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := &stack{ts: ts, sim: sim, engine: engine, cancel: cancel}
	t.Cleanup(func() { engine.Close(); cancel(); ts.Close() })
	return s
}

func (s *stack) do(t *testing.T, method, path, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, s.ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func (s *stack) waitDone(t *testing.T, id string) models.Experiment {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, b := s.do(t, "GET", "/api/v1/experiments/"+id, "", nil)
		x := decode[models.Experiment](t, b)
		if x.Status != models.StatusRunning {
			return x
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("experiment did not finish")
	return models.Experiment{}
}

func val(x models.Experiment, name string) *int64 {
	for _, m := range x.Measurements {
		if m.Name == name {
			return m.ValueMs
		}
	}
	return nil
}

func TestEndToEndAgainstSimulatedCluster(t *testing.T) {
	s := newStack(t, nil)

	code, b := s.do(t, "GET", "/api/v1/snapshot", "", nil)
	if code != 200 {
		t.Fatalf("snapshot %d %s", code, b)
	}
	snap := decode[models.Snapshot](t, b)
	if snap.Deployment == nil || snap.Deployment.Ready != 2 || len(snap.Pods) != 2 || !snap.Deployment.Converged {
		t.Fatalf("unexpected snapshot %s", b)
	}

	// Scale 2 -> 4 and back.
	for _, n := range []int{4, 2} {
		code, b = s.do(t, "POST", "/api/v1/experiments/scale", fmt.Sprintf(`{"deployment":"demo-api","replicas":%d}`, n), nil)
		if code != http.StatusAccepted {
			t.Fatalf("scale %d: %d %s", n, code, b)
		}
		x := s.waitDone(t, decode[models.Experiment](t, b).ID)
		if x.Status != models.StatusSuccess {
			t.Fatalf("scale to %d: %s %s", n, x.Status, x.StatusReason)
		}
		if val(x, "api_rtt") == nil || val(x, "time_to_convergence") == nil || x.ConvergedAt.Before(*x.RequestAcknowledgedAt) {
			t.Fatalf("measurements missing: %+v", x.Measurements)
		}
		_, b = s.do(t, "GET", "/api/v1/snapshot", "", nil)
		snap = decode[models.Snapshot](t, b)
		if snap.Deployment.Ready != int32(n) || snap.Deployment.Desired != int32(n) {
			t.Fatalf("after scale to %d snapshot shows %+v", n, snap.Deployment)
		}
	}

	// Delete one Pod and observe a replacement with a different UID.
	victim := snap.Pods[0]
	code, b = s.do(t, "POST", "/api/v1/experiments/delete-pod", fmt.Sprintf(`{"podName":%q,"confirm":true}`, victim.Name), nil)
	if code != http.StatusAccepted {
		t.Fatalf("delete-pod %d %s", code, b)
	}
	x := s.waitDone(t, decode[models.Experiment](t, b).ID)
	if x.Status != models.StatusSuccess || x.Target.ReplacementUID == "" || x.Target.ReplacementUID == string(victim.UID) {
		t.Fatalf("delete-pod result %s %+v", x.Status, x.Target)
	}
	if val(x, "time_to_replacement_ready") == nil || val(x, "time_to_desired_readiness") == nil {
		t.Fatalf("measurements %+v", x.Measurements)
	}

	_, b = s.do(t, "GET", "/api/v1/experiments", "", nil)
	hist := decode[struct{ Experiments []models.ExperimentSummary }](t, b)
	if len(hist.Experiments) != 3 || hist.Experiments[0].Type != models.TypeDeletePod {
		t.Fatalf("history %s", b)
	}
	code, b = s.do(t, "GET", "/api/v1/experiments/"+x.ID+"/export", "", nil)
	if code != 200 || !strings.Contains(string(b), `"format": "kubescope-experiments/v1"`) || !strings.Contains(string(b), "caveats") {
		t.Fatalf("export %d %s", code, b)
	}
	code, _ = s.do(t, "GET", "/api/v1/experiments/export", "", nil)
	if code != 200 {
		t.Fatal("bulk export")
	}

	// Reset to baseline (already 2) is a valid no-op experiment.
	code, b = s.do(t, "POST", "/api/v1/experiments/reset", `{"confirm":true}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("reset %d %s", code, b)
	}
	if r := s.waitDone(t, decode[models.Experiment](t, b).ID); r.Status != models.StatusSuccess || !r.Target.Reset {
		t.Fatalf("reset %s", r.Status)
	}
}

func TestSSEStreamAndReplay(t *testing.T) {
	s := newStack(t, nil)
	resp, err := http.Get(s.ts.URL + "/api/v1/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		if r, err := http.Post(s.ts.URL+"/api/v1/experiments/scale", "application/json", strings.NewReader(`{"deployment":"demo-api","replicas":3}`)); err == nil {
			r.Body.Close()
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	seen := map[string]bool{}
	var lastID string
	var prevSeq uint64
	deadline := time.After(6 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
loop:
	for {
		select {
		case <-deadline:
			break loop
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			if strings.HasPrefix(l, "id: ") {
				lastID = strings.TrimPrefix(l, "id: ")
			}
			if strings.HasPrefix(l, "data: ") {
				ev := decode[models.StreamEvent](t, []byte(strings.TrimPrefix(l, "data: ")))
				if ev.Seq != 0 {
					if ev.Seq <= prevSeq {
						t.Fatalf("non-monotonic seq %d after %d", ev.Seq, prevSeq)
					}
					prevSeq = ev.Seq
				}
				seen[ev.Type] = true
				if m, ok := ev.Data.(map[string]any); ok && ev.Type == models.StreamExperiment && m["status"] == models.StatusSuccess {
					break loop
				}
			}
		}
	}
	for _, typ := range []string{"hello", models.StreamResource, models.StreamExperiment} {
		if !seen[typ] {
			t.Fatalf("did not see %q events (saw %v)", typ, seen)
		}
	}
	// Reconnect with an old Last-Event-ID: replay from the buffer.
	req, _ := http.NewRequest("GET", s.ts.URL+"/api/v1/events/stream", nil)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp2, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	sc2 := bufio.NewScanner(resp2.Body)
	for sc2.Scan() {
		if l := sc2.Text(); strings.HasPrefix(l, "id: ") {
			if l != "id: 2" {
				t.Fatalf("replay should start at id 2, got %q (last live id %s)", l, lastID)
			}
			return
		}
	}
	t.Fatal("no replayed events")
}

func TestValidationAndErrorMapping(t *testing.T) {
	s := newStack(t, func(c *config.Config) { c.MutationsPerMinute = 120 })
	cases := []struct {
		name, method, path, body string
		hdr                      map[string]string
		code                     int
		errCode                  string
	}{
		{"bad json", "POST", "/api/v1/experiments/scale", `{"replicas":`, nil, 400, models.CodeInvalidArgument},
		{"unknown field", "POST", "/api/v1/experiments/scale", `{"replicas":3,"namespace":"kube-system"}`, nil, 400, models.CodeInvalidArgument},
		{"missing replicas", "POST", "/api/v1/experiments/scale", `{"deployment":"demo-api"}`, nil, 400, models.CodeInvalidArgument},
		{"too many replicas", "POST", "/api/v1/experiments/scale", `{"deployment":"demo-api","replicas":11}`, nil, 400, models.CodeInvalidArgument},
		{"other deployment", "POST", "/api/v1/experiments/scale", `{"deployment":"coredns","replicas":2}`, nil, 403, models.CodeForbidden},
		{"no confirm", "POST", "/api/v1/experiments/delete-pod", `{"podName":"demo-api-x"}`, nil, 400, models.CodeInvalidArgument},
		{"bad pod name", "POST", "/api/v1/experiments/delete-pod", `{"podName":"../../etc","confirm":true}`, nil, 400, models.CodeInvalidArgument},
		{"missing pod", "POST", "/api/v1/experiments/delete-pod", `{"podName":"demo-api-nope","confirm":true}`, nil, 404, models.CodeNotFound},
		{"reset without confirm", "POST", "/api/v1/experiments/reset", `{}`, nil, 400, models.CodeInvalidArgument},
		{"cross origin", "POST", "/api/v1/experiments/scale", `{"replicas":3}`, map[string]string{"Origin": "https://evil.example"}, 403, models.CodeForbidden},
		{"wrong content type", "POST", "/api/v1/experiments/scale", ``, nil, 415, models.CodeInvalidArgument},
		{"dns rebinding host", "GET", "/api/v1/snapshot", ``, map[string]string{"Host": "attacker.example:8080"}, 403, models.CodeForbidden},
		{"unknown experiment", "GET", "/api/v1/experiments/exp-nope", ``, nil, 404, models.CodeNotFound},
		{"unknown endpoint", "GET", "/api/v1/nope", ``, nil, 404, models.CodeNotFound},
		{"cancel unknown", "POST", "/api/v1/experiments/exp-nope/cancel", `{}`, nil, 404, models.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, s.ts.URL+c.path, strings.NewReader(c.body))
			if c.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for k, v := range c.hdr {
				if k == "Host" {
					req.Host = v
				} else {
					req.Header.Set(k, v)
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.code {
				t.Fatalf("status %d want %d: %s", resp.StatusCode, c.code, b)
			}
			e := decode[models.ErrorBody](t, b)
			if e.Error.Code != c.errCode || e.Error.Message == "" {
				t.Fatalf("error body %s", b)
			}
		})
	}
}

func TestConflictWhileRunning(t *testing.T) {
	s := newStack(t, nil)
	code, b := s.do(t, "POST", "/api/v1/experiments/scale", `{"deployment":"demo-api","replicas":5}`, nil)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	code, b = s.do(t, "POST", "/api/v1/experiments/scale", `{"deployment":"demo-api","replicas":3}`, nil)
	if code != 409 || decode[models.ErrorBody](t, b).Error.Code != models.CodeConflict {
		t.Fatalf("want 409, got %d %s", code, b)
	}
}

func TestRateLimit(t *testing.T) {
	s := newStack(t, func(c *config.Config) { c.MutationsPerMinute = 1 })
	got429 := false
	for i := 0; i < 15; i++ {
		code, _ := s.do(t, "POST", "/api/v1/experiments/scale", `{"replicas":99}`, nil)
		if code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("expected 429 after burst")
	}
}

func TestNotConnectedIsNotAnEmptyCluster(t *testing.T) {
	cfg, _ := config.LoadFrom(func(string) string { return "" })
	client := &kube.Client{ID: kube.Identity{Context: "missing"}, Policy: kube.PolicyFrom(cfg)}
	hub := stream.NewHub(16)
	srv := &Server{Cfg: cfg, Client: client, ClientErr: errors.New(`KUBESCOPE_CONTEXT "missing" not found in kubeconfig`),
		Engine: experiments.New(client, hub, experiments.Options{Policy: kube.PolicyFrom(cfg)}), Hub: hub}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, p := range []string{"/api/v1/snapshot"} {
		resp, _ := http.Get(ts.URL + p)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 503 || !strings.Contains(string(b), models.CodeNotConnected) {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	resp, _ := http.Post(ts.URL+"/api/v1/experiments/scale", "application/json", strings.NewReader(`{"replicas":3}`))
	if resp.StatusCode != 503 {
		t.Fatalf("scale when disconnected: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/api/v1/connection")
	b, _ := io.ReadAll(resp.Body)
	c := decode[models.Connection](t, b)
	if c.Configured || c.Reachable || c.Error == "" {
		t.Fatalf("connection should report the failure: %s", b)
	}
}

func TestClassifyKubernetesErrors(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	cases := []struct {
		err  error
		code int
	}{
		{apierrors.NewForbidden(gr, "demo-api", errors.New("rbac")), 403},
		{apierrors.NewUnauthorized("expired token"), 503},
		{apierrors.NewConflict(gr, "x", errors.New("uid")), 409},
		{apierrors.NewTimeoutError("slow", 1), 504},
		{apierrors.NewServiceUnavailable("down"), 503},
		{fmt.Errorf("Get \"https://127.0.0.1:6443\": dial tcp 127.0.0.1:6443: connect: connection refused"), 503},
		{context.DeadlineExceeded, 504},
		{errors.New("weird"), 500},
	}
	for _, c := range cases {
		if got, _ := Classify(c.err); got != c.code {
			t.Errorf("%v: got %d want %d", c.err, got, c.code)
		}
	}
}
