package httpapi

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sahilkamil/kubescope/backend/internal/config"
	"github.com/sahilkamil/kubescope/backend/internal/experiments"
	"github.com/sahilkamil/kubescope/backend/internal/kube"
	"github.com/sahilkamil/kubescope/backend/internal/stream"
	"github.com/sahilkamil/kubescope/backend/internal/testsim"
)

// TestUIHarness serves the API against the SIMULATED cluster for manual/UI
// testing of the frontend. It only runs when KUBESCOPE_UI_HARNESS_ADDR is set:
//
//	KUBESCOPE_UI_HARNESS_ADDR=127.0.0.1:18090 KUBESCOPE_STATIC_DIR=../frontend/dist \
//	  go test ./internal/httpapi -run TestUIHarness -timeout 30m
//
// The context name is "SIMULATED-fake-clientset" so screenshots taken from it
// can never be mistaken for a real cluster.
func TestUIHarness(t *testing.T) {
	addr := os.Getenv("KUBESCOPE_UI_HARNESS_ADDR")
	if addr == "" {
		t.Skip("set KUBESCOPE_UI_HARNESS_ADDR to run the UI harness")
	}
	cfg, _ := config.LoadFrom(func(string) string { return "" })
	cfg.StaticDir = os.Getenv("KUBESCOPE_STATIC_DIR")
	sim := testsim.New(cfg.Namespace, cfg.Deployment, 2)
	sim.Schedule, sim.Start, sim.Ready = 300*time.Millisecond, 600*time.Millisecond, 900*time.Millisecond
	client := kube.NewWithClientset(sim.CS, kube.Identity{Context: "SIMULATED-fake-clientset", Cluster: "SIMULATED"}, cfg)
	hub := stream.NewHub(1024)
	engine := experiments.New(client, hub, experiments.Options{Timeout: cfg.ExperimentTimeout, Identity: client.ID, Policy: kube.PolicyFrom(cfg)})
	monitor := kube.NewMonitor(client, 5*time.Second, nil)
	watcher := kube.NewWatcher(client, engine, ResourcePublisher(hub, cfg.Deployment))
	d := 10 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("KUBESCOPE_UI_HARNESS_FOR")); err == nil {
		d = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	go sim.Run(ctx)
	go watcher.Run(ctx)
	go monitor.Run(ctx)
	srv := &Server{Cfg: cfg, Client: client, Engine: engine, Hub: hub, Monitor: monitor, Watcher: watcher}
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), BaseContext: nil}
	go func() { <-ctx.Done(); _ = hs.Close() }()
	t.Logf("UI harness on http://%s (simulated cluster)", addr)
	_ = hs.ListenAndServe()
}
