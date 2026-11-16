// Package safety enforces KubeScope's backend-side guardrails: which namespace
// may be touched, which objects may be mutated, and which inputs are valid.
// The frontend also filters, but these checks are the only ones that count.
package safety

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrForbidden marks a request rejected by policy (maps to HTTP 403).
var ErrForbidden = errors.New("forbidden by KubeScope safety policy")

// ErrInvalid marks malformed input (maps to HTTP 400).
var ErrInvalid = errors.New("invalid argument")

// Policy describes what KubeScope may mutate.
type Policy struct {
	AllowedNamespaces []string
	Namespace         string
	Deployment        string
	LabelKey          string
	LabelValue        string
	MinReplicas       int32
	MaxReplicas       int32
}

func forbidden(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, a...))
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// CheckNamespace ensures ns is the selected namespace and on the allowlist.
func (p Policy) CheckNamespace(ns string) error {
	if ns != p.Namespace {
		return forbidden("namespace %q is not the selected demo namespace %q", ns, p.Namespace)
	}
	for _, a := range p.AllowedNamespaces {
		if a == ns {
			return nil
		}
	}
	return forbidden("namespace %q is not in the allowlist", ns)
}

// CheckLabels ensures an object carries the part-of label.
func (p Policy) CheckLabels(kind, name string, labels map[string]string) error {
	if labels[p.LabelKey] != p.LabelValue {
		return forbidden("%s %q is not labeled %s=%s", kind, name, p.LabelKey, p.LabelValue)
	}
	return nil
}

// ValidateDeploymentName ensures the request targets the configured demo Deployment.
func (p Policy) ValidateDeploymentName(name string) error {
	if name == "" {
		return invalid("deployment is required")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return invalid("deployment name %q is not a valid Kubernetes name", name)
	}
	if name != p.Deployment {
		return forbidden("deployment %q is not the configured demo deployment %q", name, p.Deployment)
	}
	return nil
}

// ValidateReplicas checks the requested replica count is in the safe range.
func (p Policy) ValidateReplicas(n int32) error {
	if n < p.MinReplicas || n > p.MaxReplicas {
		return invalid("replicas must be between %d and %d (got %d)", p.MinReplicas, p.MaxReplicas, n)
	}
	return nil
}

// ValidatePodName checks syntax only.
func ValidatePodName(name string) error {
	if name == "" {
		return invalid("podName is required")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return invalid("podName %q is not a valid Kubernetes name", name)
	}
	return nil
}

// CheckDeployment verifies a live Deployment may be mutated.
func (p Policy) CheckDeployment(d *appsv1.Deployment) error {
	if err := p.CheckNamespace(d.Namespace); err != nil {
		return err
	}
	if d.Name != p.Deployment {
		return forbidden("deployment %q is not the configured demo deployment", d.Name)
	}
	return p.CheckLabels("Deployment", d.Name, d.Labels)
}

// CheckPodDeletable verifies a live Pod belongs to the demo Deployment through a
// ReplicaSet owned by it, carries the label, and is not already terminating.
// rsOwner returns the owning Deployment name of a ReplicaSet ("" if unknown).
func (p Policy) CheckPodDeletable(pod *corev1.Pod, rsOwner func(rsName string) (string, error)) error {
	if err := p.CheckNamespace(pod.Namespace); err != nil {
		return err
	}
	if err := p.CheckLabels("Pod", pod.Name, pod.Labels); err != nil {
		return err
	}
	if pod.DeletionTimestamp != nil {
		return invalid("pod %q is already terminating", pod.Name)
	}
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != "ReplicaSet" {
		return forbidden("pod %q is not controlled by a ReplicaSet", pod.Name)
	}
	owner, err := rsOwner(ref.Name)
	if err != nil {
		return err
	}
	if owner != p.Deployment {
		return forbidden("pod %q does not belong to deployment %q", pod.Name, p.Deployment)
	}
	return nil
}
