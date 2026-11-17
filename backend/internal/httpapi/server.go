// Package httpapi exposes KubeScope's REST + SSE API under /api/v1.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/msahilk/kubescope-k8s-observer/backend/internal/config"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/experiments"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/kube"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/models"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/safety"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/stream"
)

// Server holds the HTTP handlers' dependencies.
type Server struct {
	Cfg       config.Config
	Client    *kube.Client // may be non-connected
	ClientErr error        // why the client could not be built
	Engine    *experiments.Engine
	Hub       *stream.Hub
	Monitor   *kube.Monitor
	Watcher   *kube.Watcher
	Log       *slog.Logger

	limiter        *rate.Limiter
	RequestTimeout time.Duration
	HeartbeatEvery time.Duration
}

// Handler builds the router with middleware.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = 30 * time.Second
	}
	if s.HeartbeatEvery == 0 {
		s.HeartbeatEvery = 15 * time.Second
	}
	perMin := s.Cfg.MutationsPerMinute
	if perMin <= 0 {
		perMin = 20
	}
	s.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(perMin)), max(10, perMin))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", s.healthz)
	mux.HandleFunc("GET /api/v1/connection", s.timeout(s.connection))
	mux.HandleFunc("GET /api/v1/snapshot", s.timeout(s.snapshot))
	mux.HandleFunc("GET /api/v1/events/stream", s.eventStream)
	mux.HandleFunc("POST /api/v1/experiments/scale", s.mutation(s.scale))
	mux.HandleFunc("POST /api/v1/experiments/reset", s.mutation(s.reset))
	mux.HandleFunc("POST /api/v1/experiments/delete-pod", s.mutation(s.deletePod))
	mux.HandleFunc("POST /api/v1/experiments/{id}/cancel", s.mutation(s.cancel))
	mux.HandleFunc("GET /api/v1/experiments", s.listExperiments)
	mux.HandleFunc("GET /api/v1/experiments/export", s.exportAll)
	mux.HandleFunc("GET /api/v1/experiments/{id}", s.getExperiment)
	mux.HandleFunc("GET /api/v1/experiments/{id}/export", s.exportOne)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, models.CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	if s.Cfg.StaticDir != "" {
		mux.Handle("/", s.static(s.Cfg.StaticDir))
	}
	return s.hostCheck(securityHeaders(mux))
}

// ---------- middleware ----------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		} else {
			h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// hostCheck rejects requests whose Host header is not a loopback name, which
// blocks DNS-rebinding attacks against the localhost-only API.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Cfg.AllowNonLoopback && !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, models.CodeForbidden, "requests must use a loopback host (localhost/127.0.0.1)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) timeout(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.RequestTimeout)
		defer cancel()
		h(w, r.WithContext(ctx))
	}
}

// mutation applies the guards every state-changing endpoint needs.
func (s *Server) mutation(h http.HandlerFunc) http.HandlerFunc {
	return s.timeout(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin, r.Host) {
			writeError(w, http.StatusForbidden, models.CodeForbidden, "cross-origin mutation rejected: "+origin)
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, models.CodeInvalidArgument, "Content-Type must be application/json")
			return
		}
		if !s.limiter.Allow() {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, models.CodeRateLimited, "too many mutating requests; slow down")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		h(w, r)
	})
}

func (s *Server) originAllowed(origin, host string) bool {
	if origin == "http://"+host || origin == "https://"+host {
		return true
	}
	for _, o := range s.Cfg.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// ---------- handlers ----------

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) connectionDTO() models.Connection {
	c := models.Connection{
		Namespace:        s.Cfg.Namespace,
		Deployment:       s.Cfg.Deployment,
		MinReplicas:      s.Cfg.MinReplicas,
		MaxReplicas:      s.Cfg.MaxReplicas,
		BaselineReplicas: s.Cfg.BaselineReplicas,
		TimeoutSeconds:   int(s.Cfg.ExperimentTimeout / time.Second),
	}
	if s.Client != nil {
		c.Context, c.Cluster, c.Server, c.IsGKE = s.Client.ID.Context, s.Client.ID.Cluster, s.Client.ID.Server, s.Client.ID.IsGKE
		c.Configured = s.Client.Connected()
	}
	if s.ClientErr != nil {
		c.Error = s.ClientErr.Error()
	}
	if s.Monitor != nil && c.Configured {
		h := s.Monitor.Health()
		c.Reachable, c.ServerVersion, c.LastSuccessAt, c.LastCheckedAt = h.Reachable, h.ServerVersion, h.LastSuccessAt, h.LastCheckedAt
		if h.Error != "" {
			c.Error = h.Error
		}
	}
	if s.Watcher != nil {
		ws := s.Watcher.Status()
		c.WatchSynced, c.WatchErrors, c.WatchRestarts, c.LastWatchError, c.LastWatchErrorAt = ws.Synced, ws.Errors, ws.Restarts, ws.LastError, ws.LastErrorAt
	}
	return c
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	if s.Monitor != nil && s.Client.Connected() && r.URL.Query().Get("refresh") == "true" {
		s.Monitor.Check(r.Context())
	}
	writeJSON(w, http.StatusOK, s.connectionDTO())
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if s.Client == nil || !s.Client.Connected() {
		msg := "not connected to a Kubernetes cluster"
		if s.ClientErr != nil {
			msg += ": " + s.ClientErr.Error()
		}
		writeError(w, http.StatusServiceUnavailable, models.CodeNotConnected, msg)
		return
	}
	seq := s.Hub.Seq()
	snap, err := s.Client.Snapshot(r.Context())
	if err != nil {
		if s.Monitor != nil && isConnectivityErr(err) {
			s.Monitor.MarkFailure(err)
		}
		s.writeErr(w, err)
		return
	}
	if s.Monitor != nil {
		s.Monitor.MarkSuccess()
	}
	snap.StreamSeq = seq
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	var req models.ScaleRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if req.Replicas == nil {
		s.writeErr(w, fmt.Errorf("%w: replicas is required", safety.ErrInvalid))
		return
	}
	if req.Deployment == "" {
		req.Deployment = s.Cfg.Deployment
	}
	if !s.requireConnected(w) {
		return
	}
	exp, err := s.Engine.StartScale(r.Context(), req.Deployment, *req.Replicas, false)
	s.respondStart(w, exp, err)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if !req.Confirm {
		s.writeErr(w, fmt.Errorf("%w: reset requires \"confirm\": true", safety.ErrInvalid))
		return
	}
	if !s.requireConnected(w) {
		return
	}
	exp, err := s.Engine.StartScale(r.Context(), s.Cfg.Deployment, s.Cfg.BaselineReplicas, true)
	s.respondStart(w, exp, err)
}

func (s *Server) deletePod(w http.ResponseWriter, r *http.Request) {
	var req models.DeletePodRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeErr(w, err)
		return
	}
	if !s.requireConnected(w) {
		return
	}
	exp, err := s.Engine.StartDeletePod(r.Context(), req.PodName, req.Confirm)
	s.respondStart(w, exp, err)
}

func (s *Server) respondStart(w http.ResponseWriter, exp models.Experiment, err error) {
	if err != nil {
		if s.Monitor != nil && isConnectivityErr(err) {
			s.Monitor.MarkFailure(err)
		}
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, exp)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Engine.Cancel(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) listExperiments(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"experiments": s.Engine.List()})
}

func (s *Server) getExperiment(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Engine.Get(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func (s *Server) exportOne(w http.ResponseWriter, r *http.Request) {
	exp, err := s.Engine.Get(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kubescope-%s.json"`, exp.ID))
	writeJSON(w, http.StatusOK, exportEnvelope([]models.Experiment{exp}))
}

func (s *Server) exportAll(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="kubescope-history.json"`)
	writeJSON(w, http.StatusOK, exportEnvelope(s.Engine.All()))
}

func exportEnvelope(exps []models.Experiment) map[string]any {
	return map[string]any{
		"format":     "kubescope-experiments/v1",
		"exportedAt": time.Now().UTC(),
		"notes": []string{
			"All timestamps are UTC and taken on the KubeScope host, not inside the Kubernetes control plane.",
			"Durations with valueMs=null were not observed; see each measurement's reason.",
			"time_to_convergence / recovery figures include scheduling, image pulls, container start and readiness probes; they are not control-plane latency.",
		},
		"experiments": exps,
	}
}

func (s *Server) requireConnected(w http.ResponseWriter) bool {
	if s.Client == nil || !s.Client.Connected() {
		msg := "not connected to a Kubernetes cluster"
		if s.ClientErr != nil {
			msg += ": " + s.ClientErr.Error()
		}
		writeError(w, http.StatusServiceUnavailable, models.CodeNotConnected, msg)
		return false
	}
	return true
}

// eventStream serves Server-Sent Events. Each message's data is a
// models.StreamEvent; the SSE id is its sequence number so EventSource sends
// Last-Event-ID on reconnect.
func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, models.CodeInternal, "streaming unsupported")
		return
	}
	var last uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	} else if v := r.URL.Query().Get("lastEventId"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	}
	sub, replay, resync := s.Hub.Subscribe(last)
	defer s.Hub.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")

	send := func(ev models.StreamEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if ev.Seq > 0 {
			if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		return err
	}
	hello := map[string]any{"seq": s.Hub.Seq(), "connection": s.connectionDTO()}
	if resync {
		_ = send(models.StreamEvent{Type: models.StreamResync, At: time.Now().UTC(), Data: map[string]any{"reason": "events were missed; refetch /snapshot", "lastEventId": last}})
	}
	_ = send(models.StreamEvent{Type: "hello", At: time.Now().UTC(), Data: hello})
	for _, ev := range replay {
		if err := send(ev); err != nil {
			return
		}
	}
	hb := time.NewTicker(s.HeartbeatEvery)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return // dropped as a slow consumer; client reconnects
			}
			if err := send(ev); err != nil {
				return
			}
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) static(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if st, err := os.Stat(p); err != nil || st.IsDir() && r.URL.Path != "/" {
			r.URL.Path = "/" // SPA fallback
		}
		fs.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("%w: request body too large", safety.ErrInvalid)
		}
		return fmt.Errorf("%w: invalid JSON body: %v", safety.ErrInvalid, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after JSON object", safety.ErrInvalid)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, models.ErrorBody{Error: models.ErrorDetail{Code: code, Message: msg}})
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	status, code := Classify(err)
	if status >= 500 && status != http.StatusServiceUnavailable && status != http.StatusGatewayTimeout {
		s.Log.Error("request failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

// Classify maps an error to an HTTP status and API error code.
func Classify(err error) (int, string) {
	switch {
	case errors.Is(err, safety.ErrInvalid):
		return http.StatusBadRequest, models.CodeInvalidArgument
	case errors.Is(err, safety.ErrForbidden):
		return http.StatusForbidden, models.CodeForbidden
	case errors.Is(err, experiments.ErrConflict), errors.Is(err, experiments.ErrNotRunning):
		return http.StatusConflict, models.CodeConflict
	case errors.Is(err, experiments.ErrNotFound):
		return http.StatusNotFound, models.CodeNotFound
	case errors.Is(err, kube.ErrNotConnected):
		return http.StatusServiceUnavailable, models.CodeNotConnected
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, models.CodeForbidden
	case apierrors.IsUnauthorized(err):
		return http.StatusServiceUnavailable, models.CodeNotConnected
	case apierrors.IsNotFound(err):
		return http.StatusNotFound, models.CodeNotFound
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return http.StatusConflict, models.CodeConflict
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return http.StatusBadRequest, models.CodeInvalidArgument
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, models.CodeTimeout
	case apierrors.IsTooManyRequests(err):
		return http.StatusTooManyRequests, models.CodeRateLimited
	case isConnectivityErr(err):
		return http.StatusServiceUnavailable, models.CodeNotConnected
	}
	return http.StatusInternalServerError, models.CodeInternal
}

func isConnectivityErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	var oe *net.OpError
	if errors.As(err, &oe) || errors.As(err, &ne) || apierrors.IsServiceUnavailable(err) || errors.Is(err, kube.ErrNotConnected) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "dial tcp")
}
