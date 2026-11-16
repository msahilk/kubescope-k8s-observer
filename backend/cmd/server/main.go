// Command server runs the KubeScope backend.
//
//	cd backend && go run ./cmd/server
//
// Configuration is via environment variables; see ../.env.example.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sahilkamil/kubescope/backend/internal/config"
	"github.com/sahilkamil/kubescope/backend/internal/experiments"
	"github.com/sahilkamil/kubescope/backend/internal/httpapi"
	"github.com/sahilkamil/kubescope/backend/internal/kube"
	"github.com/sahilkamil/kubescope/backend/internal/models"
	"github.com/sahilkamil/kubescope/backend/internal/stream"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := stream.NewHub(1024)
	client, clientErr := kube.New(cfg)
	if clientErr != nil {
		// Keep serving so the UI can show *why* it is not connected.
		log.Error("kubernetes client not configured; serving NOT_CONNECTED", "err", clientErr)
	} else {
		log.Info("kubernetes client configured", "context", client.ID.Context, "cluster", client.ID.Cluster, "server", client.ID.Server, "gke", client.ID.IsGKE, "namespace", cfg.Namespace)
		if client.ID.IsGKE {
			log.Warn("GKE context selected: experiments mutate a real cloud cluster; run them only when you intend to")
		}
	}

	engine := experiments.New(client, hub, experiments.Options{
		Timeout:  cfg.ExperimentTimeout,
		Identity: client.ID,
		Policy:   kube.PolicyFrom(cfg),
	})
	watcher := kube.NewWatcher(client, engine, httpapi.ResourcePublisher(hub, cfg.Deployment))
	var wasUnreachable atomic.Bool // onChange runs from the monitor and from HTTP handlers
	monitor := kube.NewMonitor(client, 10*time.Second, func(h kube.Health) {
		hub.Publish(models.StreamConnection, map[string]any{"reachable": h.Reachable, "error": h.Error, "checkedAt": h.LastCheckedAt})
		if !h.Reachable {
			wasUnreachable.Store(true)
			return
		}
		if wasUnreachable.CompareAndSwap(true, false) {
			// Watches may have hung silently during the outage: relist now
			// instead of waiting for client-go's timeouts, and tell clients that
			// events (e.g. deletions) may have been missed.
			log.Info("API server reachable again; restarting informers")
			watcher.Restart()
			hub.Publish(models.StreamResync, map[string]any{"reason": "API server was unreachable; watches restarted, refetch /snapshot"})
		}
	})

	if client.Connected() {
		go monitor.Run(ctx)
		go watcher.Run(ctx)
	}

	srv := &httpapi.Server{Cfg: cfg, Client: client, ClientErr: clientErr, Engine: engine, Hub: hub, Monitor: monitor, Watcher: watcher, Log: log}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// No WriteTimeout: the SSE stream is long-lived. Non-streaming handlers
		// use per-request context timeouts instead.
	}
	go func() {
		log.Info("KubeScope API listening", "addr", "http://"+cfg.Addr+"/api/v1")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	engine.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
}
