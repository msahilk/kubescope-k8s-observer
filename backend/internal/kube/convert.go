package kube

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/msahilk/kubescope-k8s-observer/backend/internal/models"
)

// Convergence is the result of evaluating a Deployment against a desired count.
type Convergence struct {
	Converged bool
	Reason    string
}

// DeploymentConverged reports whether the Deployment has fully reconciled to
// `desired` replicas. All of the following must hold:
//
//   - spec.replicas == desired (someone else has not changed the target)
//   - status.observedGeneration >= metadata.generation (the controller has seen
//     the latest spec; without this the status fields describe an older spec)
//   - status.updatedReplicas == desired (all Pods run the current template)
//   - status.readyReplicas == desired
//   - status.availableReplicas == desired (ready for minReadySeconds)
//   - status.replicas == desired (no surplus Pods from old ReplicaSets or a
//     scale-down still in progress; terminating Pods are not counted here)
//
// spec.replicas alone is never treated as success.
func DeploymentConverged(d *appsv1.Deployment, desired int32) Convergence {
	if d == nil {
		return Convergence{false, "deployment not observed"}
	}
	spec := int32(1)
	if d.Spec.Replicas != nil {
		spec = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case spec != desired:
		return Convergence{false, fmt.Sprintf("spec.replicas=%d differs from desired %d", spec, desired)}
	case s.ObservedGeneration < d.Generation:
		return Convergence{false, fmt.Sprintf("controller has not observed generation %d yet (observedGeneration=%d)", d.Generation, s.ObservedGeneration)}
	case s.UpdatedReplicas != desired:
		return Convergence{false, fmt.Sprintf("updatedReplicas=%d, want %d", s.UpdatedReplicas, desired)}
	case s.ReadyReplicas != desired:
		return Convergence{false, fmt.Sprintf("readyReplicas=%d, want %d", s.ReadyReplicas, desired)}
	case s.AvailableReplicas != desired:
		return Convergence{false, fmt.Sprintf("availableReplicas=%d, want %d", s.AvailableReplicas, desired)}
	case s.Replicas != desired:
		return Convergence{false, fmt.Sprintf("status.replicas=%d (surplus or missing Pods), want %d", s.Replicas, desired)}
	}
	return Convergence{true, fmt.Sprintf("generation %d observed; updated=ready=available=%d", d.Generation, desired)}
}

// SpecReplicas returns spec.replicas with the Kubernetes default of 1.
func SpecReplicas(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// DeploymentToInfo converts a Deployment to its DTO.
func DeploymentToInfo(d *appsv1.Deployment) *models.DeploymentInfo {
	desired := SpecReplicas(d)
	c := DeploymentConverged(d, desired)
	img := ""
	if len(d.Spec.Template.Spec.Containers) > 0 {
		img = d.Spec.Template.Spec.Containers[0].Image
	}
	return &models.DeploymentInfo{
		Name:               d.Name,
		UID:                string(d.UID),
		ResourceVersion:    d.ResourceVersion,
		Generation:         d.Generation,
		ObservedGeneration: d.Status.ObservedGeneration,
		Desired:            desired,
		Current:            d.Status.Replicas,
		Updated:            d.Status.UpdatedReplicas,
		Ready:              d.Status.ReadyReplicas,
		Available:          d.Status.AvailableReplicas,
		Unavailable:        d.Status.UnavailableReplicas,
		Image:              img,
		Converged:          c.Converged,
		ConvergenceReason:  c.Reason,
	}
}

// ReplicaSetToInfo converts a ReplicaSet to its DTO.
func ReplicaSetToInfo(rs *appsv1.ReplicaSet) *models.ReplicaSetInfo {
	desired := int32(1)
	if rs.Spec.Replicas != nil {
		desired = *rs.Spec.Replicas
	}
	return &models.ReplicaSetInfo{
		Name:            rs.Name,
		UID:             string(rs.UID),
		ResourceVersion: rs.ResourceVersion,
		Revision:        rs.Annotations["deployment.kubernetes.io/revision"],
		Desired:         desired,
		Current:         rs.Status.Replicas,
		Ready:           rs.Status.ReadyReplicas,
		Available:       rs.Status.AvailableReplicas,
	}
}

// PodReady reports the PodReady condition.
func PodReady(p *corev1.Pod) (bool, *time.Time) {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			if c.Status == corev1.ConditionTrue {
				t := c.LastTransitionTime.Time
				return true, &t
			}
			return false, nil
		}
	}
	return false, nil
}

// PodToInfo converts a Pod to its DTO.
func PodToInfo(p *corev1.Pod) *models.PodInfo {
	ready, since := PodReady(p)
	info := &models.PodInfo{
		Name:            p.Name,
		UID:             string(p.UID),
		ResourceVersion: p.ResourceVersion,
		Phase:           string(p.Status.Phase),
		Running:         p.Status.Phase == corev1.PodRunning,
		Ready:           ready,
		Terminating:     p.DeletionTimestamp != nil,
		NodeName:        p.Spec.NodeName,
		CreatedAt:       p.CreationTimestamp.Time,
		ReadySince:      since,
		Reason:          p.Status.Reason,
	}
	if ref := metav1.GetControllerOf(p); ref != nil && ref.Kind == "ReplicaSet" {
		info.ReplicaSet = ref.Name
	}
	for _, cs := range p.Status.ContainerStatuses {
		info.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && info.Reason == "" {
			info.Reason = cs.State.Waiting.Reason
		}
	}
	if info.Phase == "" {
		info.Phase = "Unknown"
	}
	return info
}

// EventToInfo converts a core/v1 Event.
func EventToInfo(e *corev1.Event) models.EventInfo {
	t := e.LastTimestamp.Time
	if t.IsZero() && !e.EventTime.IsZero() {
		t = e.EventTime.Time
	}
	if t.IsZero() {
		t = e.CreationTimestamp.Time
	}
	src := e.Source.Component
	if src == "" {
		src = e.ReportingController
	}
	count := e.Count
	if count == 0 && e.Series != nil {
		count = e.Series.Count
	}
	return models.EventInfo{
		Type:           e.Type,
		Reason:         e.Reason,
		Message:        e.Message,
		InvolvedKind:   e.InvolvedObject.Kind,
		InvolvedName:   e.InvolvedObject.Name,
		Count:          count,
		Source:         src,
		LastObservedAt: t,
	}
}

func sortPods(p []models.PodInfo) {
	sort.Slice(p, func(i, j int) bool {
		if !p[i].CreatedAt.Equal(p[j].CreatedAt) {
			return p[i].CreatedAt.Before(p[j].CreatedAt)
		}
		return p[i].Name < p[j].Name
	})
}

// DeploymentSummary is a short human string for timelines.
func DeploymentSummary(d *appsv1.Deployment) string {
	return fmt.Sprintf("gen=%d observedGen=%d spec=%d current=%d updated=%d ready=%d available=%d",
		d.Generation, d.Status.ObservedGeneration, SpecReplicas(d), d.Status.Replicas,
		d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas)
}

// PodSummary is a short human string for timelines.
func PodSummary(p *corev1.Pod) string {
	ready, _ := PodReady(p)
	s := fmt.Sprintf("phase=%s ready=%t", p.Status.Phase, ready)
	if p.Spec.NodeName != "" {
		s += " node=" + p.Spec.NodeName
	} else {
		s += " (unscheduled)"
	}
	if p.DeletionTimestamp != nil {
		s += " terminating"
	}
	return s
}
