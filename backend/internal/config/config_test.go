package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:8080" || c.Namespace != "kubescope-demo" || c.Deployment != "demo-api" ||
		c.MinReplicas != 1 || c.MaxReplicas != 5 || c.BaselineReplicas != 2 || c.ExperimentTimeout != 120*time.Second {
		t.Fatalf("unexpected defaults %+v", c)
	}
}

func TestRejectsUnsafeSettings(t *testing.T) {
	bad := []map[string]string{
		{"KUBESCOPE_ADDR": "0.0.0.0:8080"},
		{"KUBESCOPE_ADDR": "192.168.1.5:8080"},
		{"KUBESCOPE_MAX_REPLICAS": "11"},
		{"KUBESCOPE_MIN_REPLICAS": "4", "KUBESCOPE_MAX_REPLICAS": "3"},
		{"KUBESCOPE_NAMESPACE": "kube-system", "KUBESCOPE_ALLOWED_NAMESPACES": "kubescope-demo"},
		{"KUBESCOPE_EXPERIMENT_TIMEOUT": "1s"},
		{"KUBESCOPE_IN_CLUSTER": "true", "KUBESCOPE_CONTEXT": "kind-kubescope"},
		{"KUBESCOPE_BASELINE_REPLICAS": "9"},
	}
	for _, m := range bad {
		if _, err := LoadFrom(env(m)); err == nil {
			t.Fatalf("expected error for %v", m)
		}
	}
}

func TestAllowsExplicitOptIns(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"KUBESCOPE_ADDR": "0.0.0.0:9000", "KUBESCOPE_ALLOW_NON_LOOPBACK": "true", "KUBESCOPE_MAX_REPLICAS": "10", "KUBESCOPE_CONTEXT": "kind-kubescope"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxReplicas != 10 || c.Context != "kind-kubescope" {
		t.Fatalf("%+v", c)
	}
	if err := RequireLoopback("localhost:8080"); err != nil {
		t.Fatal(err)
	}
	if err := RequireLoopback("[::1]:8080"); err != nil {
		t.Fatal(err)
	}
}
