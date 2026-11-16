// Package config loads KubeScope settings from environment variables.
//
// Every setting has a safe default so `go run ./cmd/server` works against a
// local kind cluster with no extra configuration. Nothing here is ever sent to
// the browser except the non-secret values exposed through /api/v1/connection.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// HardMaxReplicas is the absolute ceiling for any scale request, regardless of
// configuration. The spec forbids high replica counts / load testing.
const HardMaxReplicas = 10

// PartOfLabelKey/PartOfLabelValue identify resources KubeScope may mutate.
const (
	PartOfLabelKey   = "app.kubernetes.io/part-of"
	PartOfLabelValue = "kubescope-demo"
)

// Config holds runtime settings.
type Config struct {
	// Addr is the HTTP listen address. Defaults to 127.0.0.1:8080 (localhost only).
	Addr string
	// AllowNonLoopback must be set to bind to a non-loopback address.
	AllowNonLoopback bool

	// Kubeconfig is an explicit kubeconfig path ("" = KUBECONFIG env / ~/.kube/config).
	Kubeconfig string
	// Context is the kubeconfig context to use. "" means the kubeconfig's current
	// context; when set, a missing context is an error (never silently switched).
	Context string
	// InCluster uses rest.InClusterConfig instead of a kubeconfig.
	InCluster bool

	// Namespace is the selected demo namespace.
	Namespace string
	// AllowedNamespaces is the allowlist the namespace must belong to.
	AllowedNamespaces []string
	// Deployment is the demo Deployment name.
	Deployment string

	MinReplicas      int32
	MaxReplicas      int32
	BaselineReplicas int32

	ExperimentTimeout time.Duration
	// MutationsPerMinute bounds POST /experiments/* calls.
	MutationsPerMinute int

	// StaticDir optionally serves a built frontend (frontend/dist).
	StaticDir string
	// AllowedOrigins are extra browser origins allowed to call mutating endpoints
	// (the Vite dev server origin is allowed by default).
	AllowedOrigins []string
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration using the supplied lookup (handy for tests).
func LoadFrom(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:               def(getenv("KUBESCOPE_ADDR"), "127.0.0.1:8080"),
		AllowNonLoopback:   getenv("KUBESCOPE_ALLOW_NON_LOOPBACK") == "true",
		Kubeconfig:         getenv("KUBECONFIG"),
		Context:            strings.TrimSpace(getenv("KUBESCOPE_CONTEXT")),
		InCluster:          getenv("KUBESCOPE_IN_CLUSTER") == "true",
		Namespace:          def(getenv("KUBESCOPE_NAMESPACE"), "kubescope-demo"),
		Deployment:         def(getenv("KUBESCOPE_DEPLOYMENT"), "demo-api"),
		StaticDir:          getenv("KUBESCOPE_STATIC_DIR"),
		AllowedOrigins:     splitCSV(def(getenv("KUBESCOPE_ALLOWED_ORIGINS"), "http://localhost:5173,http://127.0.0.1:5173")),
		MutationsPerMinute: 20,
	}
	c.AllowedNamespaces = splitCSV(def(getenv("KUBESCOPE_ALLOWED_NAMESPACES"), c.Namespace))

	var err error
	if c.MinReplicas, err = int32Env(getenv, "KUBESCOPE_MIN_REPLICAS", 1); err != nil {
		return c, err
	}
	if c.MaxReplicas, err = int32Env(getenv, "KUBESCOPE_MAX_REPLICAS", 5); err != nil {
		return c, err
	}
	if c.BaselineReplicas, err = int32Env(getenv, "KUBESCOPE_BASELINE_REPLICAS", 2); err != nil {
		return c, err
	}
	if v := getenv("KUBESCOPE_MUTATIONS_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 120 {
			return c, fmt.Errorf("KUBESCOPE_MUTATIONS_PER_MINUTE must be 1..120")
		}
		c.MutationsPerMinute = n
	}
	c.ExperimentTimeout = 120 * time.Second
	if v := getenv("KUBESCOPE_EXPERIMENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 5*time.Second || d > 15*time.Minute {
			return c, fmt.Errorf("KUBESCOPE_EXPERIMENT_TIMEOUT must be a duration between 5s and 15m")
		}
		c.ExperimentTimeout = d
	}
	return c, c.Validate()
}

// Validate checks invariants.
func (c Config) Validate() error {
	if c.MinReplicas < 0 || c.MaxReplicas < c.MinReplicas {
		return fmt.Errorf("invalid replica bounds: min=%d max=%d", c.MinReplicas, c.MaxReplicas)
	}
	if c.MaxReplicas > HardMaxReplicas {
		return fmt.Errorf("KUBESCOPE_MAX_REPLICAS=%d exceeds hard limit %d", c.MaxReplicas, HardMaxReplicas)
	}
	if c.BaselineReplicas < c.MinReplicas || c.BaselineReplicas > c.MaxReplicas {
		return fmt.Errorf("KUBESCOPE_BASELINE_REPLICAS=%d outside [%d,%d]", c.BaselineReplicas, c.MinReplicas, c.MaxReplicas)
	}
	found := false
	for _, ns := range c.AllowedNamespaces {
		if ns == c.Namespace {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("namespace %q is not in KUBESCOPE_ALLOWED_NAMESPACES %v", c.Namespace, c.AllowedNamespaces)
	}
	if c.InCluster && c.Context != "" {
		return fmt.Errorf("KUBESCOPE_CONTEXT cannot be combined with KUBESCOPE_IN_CLUSTER")
	}
	if !c.AllowNonLoopback {
		if err := RequireLoopback(c.Addr); err != nil {
			return err
		}
	}
	return nil
}

// RequireLoopback rejects listen addresses that are not loopback-only.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid KUBESCOPE_ADDR %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("KUBESCOPE_ADDR %q is not a loopback address; mutation endpoints have no authentication. "+
			"Set KUBESCOPE_ALLOW_NON_LOOPBACK=true only behind your own authentication", addr)
	}
	return nil
}

func def(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return strings.TrimSpace(v)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func int32Env(getenv func(string) string, key string, d int32) (int32, error) {
	v := getenv(key)
	if v == "" {
		return d, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return int32(n), nil
}
