package httpapi

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/msahilk/kubescope-k8s-observer/backend/internal/kube"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/models"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/stream"
)

// ResourcePublisher converts watch observations into SSE events. Only objects
// belonging to the demo Deployment are published.
func ResourcePublisher(hub *stream.Hub, deployment string) kube.Observer {
	prefix := deployment + "-"
	owned := func(name string) bool {
		return name == deployment || len(name) > len(prefix) && name[:len(prefix)] == prefix
	}
	return kube.ObserverFunc(func(o kube.Observation) {
		switch obj := o.Obj.(type) {
		case *appsv1.Deployment:
			if obj.Name != deployment {
				return
			}
			hub.Publish(models.StreamResource, models.ResourceChange{
				Kind: "Deployment", Action: o.Action, Name: obj.Name, UID: string(obj.UID), ResourceVersion: obj.ResourceVersion,
				Summary: kube.DeploymentSummary(obj), Deployment: kube.DeploymentToInfo(obj),
			})
		case *appsv1.ReplicaSet:
			if ref := metav1.GetControllerOf(obj); ref == nil || ref.Name != deployment {
				return
			}
			info := kube.ReplicaSetToInfo(obj)
			hub.Publish(models.StreamResource, models.ResourceChange{
				Kind: "ReplicaSet", Action: o.Action, Name: obj.Name, UID: string(obj.UID), ResourceVersion: obj.ResourceVersion,
				Summary: "replicas desired/current/ready = " + itoa(info.Desired) + "/" + itoa(info.Current) + "/" + itoa(info.Ready), ReplicaSet: info,
			})
		case *corev1.Pod:
			ref := metav1.GetControllerOf(obj)
			if ref == nil || !owned(ref.Name) {
				return
			}
			hub.Publish(models.StreamResource, models.ResourceChange{
				Kind: "Pod", Action: o.Action, Name: obj.Name, UID: string(obj.UID), ResourceVersion: obj.ResourceVersion,
				Summary: kube.PodSummary(obj), Pod: kube.PodToInfo(obj),
			})
		case *corev1.Event:
			if !owned(obj.InvolvedObject.Name) {
				return
			}
			hub.Publish(models.StreamK8sEvent, kube.EventToInfo(obj))
		}
	})
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
