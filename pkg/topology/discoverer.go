// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package topology

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Discoverer is an informer-backed cluster inspector. Discovery is strictly
// read-only. Run starts the shared informer factory; callers must call it
// before Snapshot.
type Discoverer struct {
	client    kubernetes.Interface
	factory   informers.SharedInformerFactory
	resync    time.Duration
	mu        sync.Mutex
	started   bool
	stopCh    chan struct{}
	syncCheck []cache.InformerSynced
}

// New constructs a Discoverer with cluster-scoped informers. Per-namespace
// filtering happens at Snapshot time so adding/removing arenas needs no
// re-bootstrap.
func New(client kubernetes.Interface, resync time.Duration) *Discoverer {
	if resync <= 0 {
		resync = 30 * time.Second
	}
	d := &Discoverer{
		client: client,
		resync: resync,
		stopCh: make(chan struct{}),
	}
	d.factory = informers.NewSharedInformerFactory(client, resync)
	// Touch the informers we use so the factory knows to start them.
	d.syncCheck = []cache.InformerSynced{
		d.factory.Core().V1().Pods().Informer().HasSynced,
		d.factory.Core().V1().Services().Informer().HasSynced,
		d.factory.Core().V1().Events().Informer().HasSynced,
		d.factory.Apps().V1().Deployments().Informer().HasSynced,
		d.factory.Apps().V1().StatefulSets().Informer().HasSynced,
		d.factory.Apps().V1().DaemonSets().Informer().HasSynced,
		d.factory.Networking().V1().NetworkPolicies().Informer().HasSynced,
	}
	return d
}

// Run starts the informer factory, waits for cache sync, then blocks until
// ctx is done. Callers typically run it in its own goroutine.
func (d *Discoverer) Run(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return fmt.Errorf("topology: Run already called")
	}
	d.started = true
	d.mu.Unlock()

	d.factory.Start(d.stopCh)
	if !cache.WaitForCacheSync(ctx.Done(), d.syncCheck...) {
		return fmt.Errorf("topology: cache sync canceled before completion")
	}
	<-ctx.Done()
	return nil
}

// Stop shuts down the informer factory. Safe from any goroutine; idempotent.
func (d *Discoverer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
}

// WaitForSync blocks until all watched caches have synced, ctx is done, or
// the discoverer is stopped. Useful for tests that want to call Snapshot
// deterministically immediately after Start.
func (d *Discoverer) WaitForSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), d.syncCheck...)
}

// Start kicks the informer factory without blocking. Tests that call
// Snapshot synchronously use this + WaitForSync; the long-running serve.go
// path uses Run instead.
func (d *Discoverer) Start() {
	d.mu.Lock()
	if !d.started {
		d.started = true
		d.factory.Start(d.stopCh)
	}
	d.mu.Unlock()
}

// Snapshot returns a TargetTopology for the given namespace. Reads from the
// informer caches — no API round-trips.
func (d *Discoverer) Snapshot(_ context.Context, ns string) (*TargetTopology, error) {
	if ns == "" {
		return nil, fmt.Errorf("topology: namespace is required")
	}

	out := &TargetTopology{
		Namespace:       ns,
		DiscoveredAt:    time.Now().UTC(),
		DependencyGraph: map[string][]string{},
		ReplicaMap:      map[string]int32{},
		PodStatus:       map[string][]PodSummary{},
		EdgeProvenance:  map[string][]string{},
	}

	depLister := d.factory.Apps().V1().Deployments().Lister().Deployments(ns)
	stsLister := d.factory.Apps().V1().StatefulSets().Lister().StatefulSets(ns)
	dsLister := d.factory.Apps().V1().DaemonSets().Lister().DaemonSets(ns)
	svcLister := d.factory.Core().V1().Services().Lister().Services(ns)
	podLister := d.factory.Core().V1().Pods().Lister().Pods(ns)
	evtLister := d.factory.Core().V1().Events().Lister().Events(ns)
	npLister := d.factory.Networking().V1().NetworkPolicies().Lister().NetworkPolicies(ns)

	deps, err := depLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("topology: list deployments: %w", err)
	}
	for _, d2 := range deps {
		w := workloadFromDeployment(d2)
		out.Workloads = append(out.Workloads, w)
		out.ReplicaMap[w.Name] = w.DesiredReplicas
	}

	stss, err := stsLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("topology: list statefulsets: %w", err)
	}
	for _, s := range stss {
		w := workloadFromStatefulSet(s)
		out.Workloads = append(out.Workloads, w)
		out.ReplicaMap[w.Name] = w.DesiredReplicas
	}

	dss, err := dsLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("topology: list daemonsets: %w", err)
	}
	for _, d2 := range dss {
		w := workloadFromDaemonSet(d2)
		out.Workloads = append(out.Workloads, w)
		out.ReplicaMap[w.Name] = w.DesiredReplicas
	}

	svcs, err := svcLister.List(labels.Everything())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("topology: list services: %w", err)
	}
	for _, s := range svcs {
		out.Services = append(out.Services, serviceSummary(s))
	}

	pods, err := podLister.List(labels.Everything())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("topology: list pods: %w", err)
	}
	out.PodStatus = groupPodsByOwner(pods)

	if evts, err := evtLister.List(labels.Everything()); err == nil {
		out.RecentEvents = recentEvents(evts, 25)
	}

	nps, err := npLister.List(labels.Everything())
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("topology: list networkpolicies: %w", err)
	}
	npStructs := make([]netv1.NetworkPolicy, 0, len(nps))
	for _, np := range nps {
		npStructs = append(npStructs, *np)
	}

	MergeEdges(out.DependencyGraph, out.EdgeProvenance,
		EdgesFromNetworkPolicies(out.Workloads, npStructs), "networkpolicy")
	MergeEdges(out.DependencyGraph, out.EdgeProvenance,
		EdgesFromEnvVars(out.Workloads, out.Services), "envvar")

	sort.Slice(out.Workloads, func(i, j int) bool {
		if out.Workloads[i].Kind != out.Workloads[j].Kind {
			return out.Workloads[i].Kind < out.Workloads[j].Kind
		}
		return out.Workloads[i].Name < out.Workloads[j].Name
	})
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].Name < out.Services[j].Name })

	return out, nil
}

func workloadFromDeployment(d *appsv1.Deployment) Workload {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	w := Workload{
		Kind:            "Deployment",
		Name:            d.Name,
		Labels:          copyMap(d.Spec.Template.Labels),
		DesiredReplicas: desired,
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		w.Containers = append(w.Containers, ContainerSummary{
			Name:    c.Name,
			Image:   c.Image,
			EnvRefs: envRefsFromContainer(c),
		})
	}
	return w
}

func workloadFromStatefulSet(s *appsv1.StatefulSet) Workload {
	desired := int32(1)
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	w := Workload{
		Kind:            "StatefulSet",
		Name:            s.Name,
		Labels:          copyMap(s.Spec.Template.Labels),
		DesiredReplicas: desired,
	}
	for _, c := range s.Spec.Template.Spec.Containers {
		w.Containers = append(w.Containers, ContainerSummary{
			Name:    c.Name,
			Image:   c.Image,
			EnvRefs: envRefsFromContainer(c),
		})
	}
	return w
}

func workloadFromDaemonSet(d *appsv1.DaemonSet) Workload {
	w := Workload{
		Kind:            "DaemonSet",
		Name:            d.Name,
		Labels:          copyMap(d.Spec.Template.Labels),
		DesiredReplicas: d.Status.DesiredNumberScheduled,
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		w.Containers = append(w.Containers, ContainerSummary{
			Name:    c.Name,
			Image:   c.Image,
			EnvRefs: envRefsFromContainer(c),
		})
	}
	return w
}

func serviceSummary(s *corev1.Service) Service {
	out := Service{
		Name:     s.Name,
		Selector: copyMap(s.Spec.Selector),
	}
	for _, p := range s.Spec.Ports {
		out.Ports = append(out.Ports, ServicePort{Name: p.Name, Port: p.Port})
	}
	return out
}

// groupPodsByOwner buckets pods by their controller's name (the part of an
// owner-reference's name before the last dash for ReplicaSets, since Pod
// owners are RSes, not Deployments). Best-effort.
func groupPodsByOwner(pods []*corev1.Pod) map[string][]PodSummary {
	out := map[string][]PodSummary{}
	now := time.Now().UTC()
	for _, p := range pods {
		owner := podOwner(p)
		out[owner] = append(out[owner], PodSummary{
			Name:     p.Name,
			Phase:    string(p.Status.Phase),
			Ready:    podIsReady(p),
			Restarts: totalRestarts(p),
			NodeName: p.Spec.NodeName,
			AgeSec:   int64(now.Sub(p.CreationTimestamp.Time).Seconds()),
		})
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i].Name < out[k][j].Name })
	}
	return out
}

func podOwner(p *corev1.Pod) string {
	for _, ref := range p.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			n := ref.Name
			if i := lastDashIndex(n); i > 0 {
				return n[:i]
			}
			return n
		case "StatefulSet", "DaemonSet", "Job":
			return ref.Name
		}
	}
	return p.Name
}

func lastDashIndex(s string) int {
	out := -1
	for i, r := range s {
		if r == '-' {
			out = i
		}
	}
	return out
}

func podIsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func totalRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

func recentEvents(evts []*corev1.Event, max int) []EventSummary {
	type sortable struct {
		t  time.Time
		ev EventSummary
	}
	entries := make([]sortable, 0, len(evts))
	for _, e := range evts {
		t := e.LastTimestamp.Time
		if t.IsZero() {
			t = e.EventTime.Time
		}
		if t.IsZero() {
			t = e.CreationTimestamp.Time
		}
		entries = append(entries, sortable{
			t: t,
			ev: EventSummary{
				Time:           t.UTC(),
				Type:           e.Type,
				Reason:         e.Reason,
				Message:        e.Message,
				InvolvedObject: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			},
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.After(entries[j].t) })
	if len(entries) > max {
		entries = entries[:max]
	}
	out := make([]EventSummary, len(entries))
	for i, e := range entries {
		out[i] = e.ev
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
