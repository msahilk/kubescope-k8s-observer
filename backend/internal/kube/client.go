// Package kube wraps client-go: building a client from kubeconfig with an
// explicit context, read-only snapshots, the two allowed mutations, and the
// informer-based watcher.
package kube

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/msahilk/kubescope-k8s-observer/backend/internal/config"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/models"
	"github.com/msahilk/kubescope-k8s-observer/backend/internal/safety"
)

// ErrNotConnected is returned when no usable client could be built.
var ErrNotConnected = errors.New("not connected to a Kubernetes cluster")

// Identity is non-secret information about the selected cluster.
type Identity struct {
	Context string
	Cluster string
	Server  string
	IsGKE   bool
}

// Client is the KubeScope view of one cluster/namespace/deployment.
type Client struct {
	CS       kubernetes.Interface
	ID       Identity
	Policy   safety.Policy
	Timeout  time.Duration // per API call
	labelSel string
}

// BuildRESTConfig resolves a rest.Config honoring KUBESCOPE_CONTEXT strictly.
func BuildRESTConfig(cfg config.Config) (*rest.Config, Identity, error) {
	if cfg.InCluster {
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, Identity{}, fmt.Errorf("in-cluster config: %w", err)
		}
		return rc, Identity{Context: "in-cluster", Cluster: "in-cluster", Server: hostOnly(rc.Host)}, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = ""
		rules.Precedence = splitKubeconfigPaths(cfg.Kubeconfig)
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, Identity{}, fmt.Errorf("loading kubeconfig: %w", err)
	}
	ctxName := raw.CurrentContext
	if cfg.Context != "" {
		if _, ok := raw.Contexts[cfg.Context]; !ok {
			names := make([]string, 0, len(raw.Contexts))
			for n := range raw.Contexts {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, Identity{Context: cfg.Context}, fmt.Errorf("KUBESCOPE_CONTEXT %q not found in kubeconfig (available: %s)", cfg.Context, strings.Join(names, ", "))
		}
		ctxName = cfg.Context
	}
	if ctxName == "" {
		return nil, Identity{}, fmt.Errorf("kubeconfig has no current context; set KUBESCOPE_CONTEXT")
	}
	kctx := raw.Contexts[ctxName]
	if kctx == nil {
		return nil, Identity{Context: ctxName}, fmt.Errorf("context %q not found in kubeconfig", ctxName)
	}
	id := Identity{Context: ctxName, Cluster: kctx.Cluster}
	if cl := raw.Clusters[kctx.Cluster]; cl != nil {
		id.Server = hostOnly(cl.Server)
	}
	if ai := raw.AuthInfos[kctx.AuthInfo]; ai != nil && ai.Exec != nil && strings.Contains(ai.Exec.Command, "gke-gcloud-auth-plugin") {
		id.IsGKE = true
	}
	if strings.HasPrefix(ctxName, "gke_") || strings.HasPrefix(kctx.Cluster, "gke_") {
		id.IsGKE = true
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*raw, ctxName, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, id, fmt.Errorf("building client config for context %q: %w", ctxName, err)
	}
	return rc, id, nil
}

func splitKubeconfigPaths(p string) []string {
	// KUBECONFIG may be a list separated by ':' (or ';' on Windows).
	sep := ":"
	if strings.Contains(p, ";") {
		sep = ";"
	}
	var out []string
	for _, s := range strings.Split(p, sep) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func hostOnly(server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return server
	}
	return u.Scheme + "://" + u.Host
}

// New builds a Client from configuration.
func New(cfg config.Config) (*Client, error) {
	rc, id, err := BuildRESTConfig(cfg)
	if err != nil {
		return &Client{ID: id, Policy: PolicyFrom(cfg)}, err
	}
	rc.Timeout = 30 * time.Second
	rc.QPS, rc.Burst = 20, 40
	rc.UserAgent = "kubescope/0.1"
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return &Client{ID: id, Policy: PolicyFrom(cfg)}, fmt.Errorf("creating clientset: %w", err)
	}
	return NewWithClientset(cs, id, cfg), nil
}

// NewWithClientset wires an existing clientset (used by tests with the fake clientset).
func NewWithClientset(cs kubernetes.Interface, id Identity, cfg config.Config) *Client {
	return &Client{
		CS:       cs,
		ID:       id,
		Policy:   PolicyFrom(cfg),
		Timeout:  10 * time.Second,
		labelSel: config.PartOfLabelKey + "=" + config.PartOfLabelValue,
	}
}

// PolicyFrom derives the safety policy from config.
func PolicyFrom(cfg config.Config) safety.Policy {
	return safety.Policy{
		AllowedNamespaces: cfg.AllowedNamespaces,
		Namespace:         cfg.Namespace,
		Deployment:        cfg.Deployment,
		LabelKey:          config.PartOfLabelKey,
		LabelValue:        config.PartOfLabelValue,
		MinReplicas:       cfg.MinReplicas,
		MaxReplicas:       cfg.MaxReplicas,
	}
}

// Connected reports whether a clientset exists.
func (c *Client) Connected() bool { return c != nil && c.CS != nil }

// LabelSelector returns the part-of selector used for every list/watch.
func (c *Client) LabelSelector() string { return c.labelSel }

func (c *Client) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, c.Timeout)
}

// Ping checks API reachability (GET /version).
func (c *Client) Ping(ctx context.Context) (string, error) {
	if !c.Connected() {
		return "", ErrNotConnected
	}
	type result struct {
		v   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := c.CS.Discovery().ServerVersion()
		if err != nil {
			ch <- result{"", err}
			return
		}
		ch <- result{v.GitVersion, nil}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// GetDeployment fetches the demo Deployment.
func (c *Client) GetDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	if !c.Connected() {
		return nil, ErrNotConnected
	}
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.CS.AppsV1().Deployments(c.Policy.Namespace).Get(cctx, c.Policy.Deployment, metav1.GetOptions{})
}

// PatchReplicas sets spec.replicas with a JSON merge patch after re-checking policy.
func (c *Client) PatchReplicas(ctx context.Context, replicas int32) (*appsv1.Deployment, error) {
	if !c.Connected() {
		return nil, ErrNotConnected
	}
	if err := c.Policy.ValidateReplicas(replicas); err != nil {
		return nil, err
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.CS.AppsV1().Deployments(c.Policy.Namespace).Patch(cctx, c.Policy.Deployment, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "kubescope"})
}

// GetPod fetches a Pod in the demo namespace.
func (c *Client) GetPod(ctx context.Context, name string) (*corev1.Pod, error) {
	if !c.Connected() {
		return nil, ErrNotConnected
	}
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.CS.CoreV1().Pods(c.Policy.Namespace).Get(cctx, name, metav1.GetOptions{})
}

// ReplicaSetOwner returns the Deployment controlling a ReplicaSet in the demo namespace.
func (c *Client) ReplicaSetOwner(ctx context.Context, rsName string) (string, error) {
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	rs, err := c.CS.AppsV1().ReplicaSets(c.Policy.Namespace).Get(cctx, rsName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if ref := metav1.GetControllerOf(rs); ref != nil && ref.Kind == "Deployment" {
		return ref.Name, nil
	}
	return "", nil
}

// DeletePod deletes a Pod by name with a UID precondition, so a same-named
// replacement can never be deleted by accident.
func (c *Client) DeletePod(ctx context.Context, name string, uid types.UID) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.CS.CoreV1().Pods(c.Policy.Namespace).Delete(cctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
}

// Snapshot lists the demo resources directly from the API server (not from a
// cache), so a successful snapshot is also proof of current reachability.
func (c *Client) Snapshot(ctx context.Context) (*models.Snapshot, error) {
	if !c.Connected() {
		return nil, ErrNotConnected
	}
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	ns := c.Policy.Namespace
	snap := &models.Snapshot{Namespace: ns, ReplicaSets: []models.ReplicaSetInfo{}, Pods: []models.PodInfo{}, Events: []models.EventInfo{}}

	dep, err := c.CS.AppsV1().Deployments(ns).Get(cctx, c.Policy.Deployment, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("Deployment %s/%s not found. Apply k8s/demo-deployment.yaml.", ns, c.Policy.Deployment))
	case err != nil:
		return nil, err
	default:
		if err := c.Policy.CheckLabels("Deployment", dep.Name, dep.Labels); err != nil {
			snap.Warnings = append(snap.Warnings, err.Error()+" — mutations will be refused.")
		}
		snap.Deployment = DeploymentToInfo(dep)
	}

	rsl, err := c.CS.AppsV1().ReplicaSets(ns).List(cctx, metav1.ListOptions{LabelSelector: c.labelSel})
	if err != nil {
		return nil, err
	}
	names := map[string]bool{c.Policy.Deployment: true}
	for i := range rsl.Items {
		rs := &rsl.Items[i]
		if ref := metav1.GetControllerOf(rs); ref == nil || ref.Name != c.Policy.Deployment {
			continue
		}
		names[rs.Name] = true
		snap.ReplicaSets = append(snap.ReplicaSets, *ReplicaSetToInfo(rs))
	}
	pl, err := c.CS.CoreV1().Pods(ns).List(cctx, metav1.ListOptions{LabelSelector: c.labelSel})
	if err != nil {
		return nil, err
	}
	for i := range pl.Items {
		p := &pl.Items[i]
		if ref := metav1.GetControllerOf(p); ref == nil || !names[ref.Name] {
			continue
		}
		names[p.Name] = true
		snap.Pods = append(snap.Pods, *PodToInfo(p))
	}
	sortPods(snap.Pods)

	el, err := c.CS.CoreV1().Events(ns).List(cctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			snap.Warnings = append(snap.Warnings, "Listing Events is forbidden for this identity; the timeline will omit Kubernetes Events.")
		} else {
			return nil, err
		}
	} else {
		for i := range el.Items {
			e := &el.Items[i]
			if names[e.InvolvedObject.Name] {
				snap.Events = append(snap.Events, EventToInfo(e))
			}
		}
		sort.Slice(snap.Events, func(i, j int) bool { return snap.Events[i].LastObservedAt.After(snap.Events[j].LastObservedAt) })
		if len(snap.Events) > 50 {
			snap.Events = snap.Events[:50]
		}
	}
	snap.ObservedAt = time.Now().UTC()
	return snap, nil
}

// ListPods lists labeled Pods in the demo namespace.
func (c *Client) ListPods(ctx context.Context) ([]corev1.Pod, error) {
	if !c.Connected() {
		return nil, ErrNotConnected
	}
	cctx, cancel := c.ctx(ctx)
	defer cancel()
	l, err := c.CS.CoreV1().Pods(c.Policy.Namespace).List(cctx, metav1.ListOptions{LabelSelector: c.labelSel})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}
