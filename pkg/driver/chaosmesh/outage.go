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

package chaosmesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// ZoneOutage and NodeOutage (#258) are composite kinds: no CRD of their own,
// but a bundle of PodChaos pod-failure objects, one per arena workload with
// pods in the zone or on the node, each selecting that workload's pods there
// and nothing else. Nothing of the operator's is modified; the pods stay
// scheduled and stop running for the fault's duration, as in the first
// minutes of a real outage.
//
// Every member carries the bundle label, so Clear, ConfirmInjected, the
// orphan reaper and ListLive find them all from the bundle's engine UID or
// from the cluster alone, and a restarted controller adopts the outage as one
// fault.

// Labels and annotations on outage members.
const (
	// BundleLabel carries the bundle ID every member of one outage shares.
	BundleLabel = "simian.chaos/bundle"
	// outageKindAnnotation is the composite kind, so ListLive can report the
	// outage as what it is rather than as PodChaos.
	outageKindAnnotation = "simian.chaos/outage-kind"
	// outageSpecAnnotation is the composite spec, JSON-encoded.
	outageSpecAnnotation = "simian.chaos/outage-spec"
	// bundleSizeAnnotation is how many members the bundle was created with,
	// so a confirmation can tell a missing member from a complete set.
	bundleSizeAnnotation = "simian.chaos/bundle-size"
	// workloadAnnotation names the workload a member selects, "Kind/name".
	workloadAnnotation = "simian.chaos/outage-workload"
)

// bundlePrefix starts a bundle's engine UID. A namespace cannot contain a
// colon, so it cannot be mistaken for a single object's "<ns>/<name>@...".
const bundlePrefix = "bundle:"

// WithKubernetes gives the driver the core and apps clients the outage kinds
// need to find nodes, pods and workloads. Without it they are not offered in
// the catalog and refused at apply. Returns d, for chaining onto New.
func (d *Driver) WithKubernetes(k kubernetes.Interface) *Driver {
	d.kube = k
	return d
}

// bundleUID encodes a bundle as "bundle:<ns>/<id>@group/version/resource".
func bundleUID(namespace, id string, gvr schema.GroupVersionResource) string {
	return bundlePrefix + engineUID(namespace, id, gvr)
}

// decodeBundleUID reports whether s is a bundle's engine UID and, if so, its
// parts.
func decodeBundleUID(s string) (ns, id string, gvr schema.GroupVersionResource, ok bool, err error) {
	rest, isBundle := strings.CutPrefix(s, bundlePrefix)
	if !isBundle {
		return "", "", schema.GroupVersionResource{}, false, nil
	}
	ns, id, gvr, err = decodeEngineUID(rest)
	return ns, id, gvr, true, err
}

// outageWorkload is one arena workload an outage reaches.
type outageWorkload struct {
	Kind     string
	Name     string
	selector *metav1.LabelSelector
	// Pods are the workload's pods in the zone or on the node.
	Pods []string
	// Total is how many pods the workload has anywhere.
	Total int
}

func (w outageWorkload) ref() string { return w.Kind + "/" + w.Name }

// outagePlan is what an outage would act on, read from the cluster at one
// moment.
type outagePlan struct {
	Namespace string
	Kind      string
	Field     string // "zone" | "node"
	Place     string
	Nodes     []string
	Workloads []outageWorkload
	// Excluded are excluded workloads with pods there, which are skipped.
	Excluded []string
	// Affected is every pod the members' selectors reach, sorted.
	Affected []string
}

// fullyDown lists the workloads every one of whose pods is in the place:
// those go down entirely, not only partly.
func (p *outagePlan) fullyDown() []string {
	var out []string
	for _, w := range p.Workloads {
		if len(w.Pods) == w.Total {
			out = append(out, w.ref())
		}
	}
	return out
}

// auditDetail is what driver.applied records about the outage.
func (p *outagePlan) auditDetail(objects map[string]string) map[string]any {
	workloads := make([]any, 0, len(p.Workloads))
	for _, w := range p.Workloads {
		entry := map[string]any{
			"kind":       w.Kind,
			"name":       w.Name,
			"pods":       w.Pods,
			"pods_total": w.Total,
			"fully_down": len(w.Pods) == w.Total,
		}
		if obj := objects[w.ref()]; obj != "" {
			entry["object"] = obj
		}
		workloads = append(workloads, entry)
	}
	out := map[string]any{
		p.Field:         p.Place,
		"action":        catalog.OutageActionPodFailure,
		"workloads":     workloads,
		"pods_affected": len(p.Affected),
		"pods":          p.Affected,
	}
	if p.Field == "zone" {
		out["nodes"] = p.Nodes
	}
	if len(p.Excluded) > 0 {
		out["excluded_skipped"] = p.Excluded
	}
	if down := p.fullyDown(); len(down) > 0 {
		// Called out on its own: the rest of the outage degrades a
		// workload, this takes it out.
		out["fully_down"] = down
	}
	return out
}

func incompatible(format string, args ...any) error {
	return fmt.Errorf("%w: %s", simian.ErrTargetIncompatible, fmt.Sprintf(format, args...))
}

// CheckTargets implements simian.TargetChecker: an outage is refused before it
// is applied when there is nothing of the arena's in the zone or on the node.
func (d *Driver) CheckTargets(ctx context.Context, m simian.FaultManifest) error {
	if !catalog.IsOutageKind(m.Engine, m.ResourceKind) {
		return nil
	}
	_, err := d.planOutage(ctx, m)
	return err
}

// planOutage reads the cluster and returns what the outage would act on now.
func (d *Driver) planOutage(ctx context.Context, m simian.FaultManifest) (*outagePlan, error) {
	if err := catalog.CheckOutageManifest(m); err != nil {
		return nil, err
	}
	if d.kube == nil {
		return nil, fmt.Errorf("%s needs a Kubernetes client to find nodes and pods; this driver was built without one", m.ResourceKind)
	}
	field, place := catalog.OutagePlace(m)
	plan := &outagePlan{Namespace: m.Targets[0].Namespace, Kind: m.ResourceKind, Field: field, Place: place}

	nodes, err := d.outageNodes(ctx, plan)
	if err != nil {
		return nil, err
	}
	plan.Nodes = nodes

	pods, err := d.kube.CoreV1().Pods(plan.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", plan.Namespace, err)
	}
	var live []corev1.Pod
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			live = append(live, p)
		}
	}
	there := func(p corev1.Pod) bool { return slices.Contains(nodes, p.Spec.NodeName) }

	workloads, err := d.arenaWorkloads(ctx, plan.Namespace)
	if err != nil {
		return nil, err
	}
	excluded, err := d.excludedWorkloads(ctx, plan.Namespace)
	if err != nil {
		return nil, err
	}
	var named []string
	for _, t := range m.Targets {
		if t.Name != "" {
			named = append(named, t.Name)
		}
	}

	owner := map[string]string{} // pod name -> workload ref, for pods in the place
	excludedRefs := map[string]bool{}
	for i := range workloads {
		w := &workloads[i]
		for _, p := range live {
			if !owns(*w, p) {
				continue
			}
			w.Total++
			if there(p) {
				w.Pods = append(w.Pods, p.Name)
				owner[p.Name] = w.ref()
			}
		}
		if len(w.Pods) == 0 {
			continue
		}
		switch {
		case slices.Contains(excluded, w.Name):
			plan.Excluded = append(plan.Excluded, w.ref())
			excludedRefs[w.ref()] = true
		case len(named) > 0 && !slices.Contains(named, w.Name):
			// The targets name workloads: the outage is limited to them.
		default:
			sort.Strings(w.Pods)
			plan.Workloads = append(plan.Workloads, *w)
		}
	}
	sort.Strings(plan.Excluded)

	if len(plan.Workloads) == 0 {
		what := "no workload of arena " + plan.Namespace
		if len(named) > 0 {
			what = fmt.Sprintf("none of %s in arena %s", strings.Join(named, ", "), plan.Namespace)
		}
		msg := fmt.Sprintf("%s has pods %s, so %s would take nothing down", what, plan.where(), m.ResourceKind)
		if len(plan.Excluded) > 0 {
			msg += fmt.Sprintf(" (excluded, and skipped: %s)", strings.Join(plan.Excluded, ", "))
		}
		return nil, incompatible("%s", msg)
	}

	// What each member's selector reaches, which is wider than the workload
	// it is for when another workload's pods carry the same labels. That is
	// harmless for a workload going down anyway, and not for an excluded one.
	affected := map[string]bool{}
	for _, w := range plan.Workloads {
		sel, err := metav1.LabelSelectorAsSelector(w.selector)
		if err != nil {
			return nil, fmt.Errorf("%s selector: %w", w.ref(), err)
		}
		for _, p := range live {
			if !there(p) || !sel.Matches(labels.Set(p.Labels)) {
				continue
			}
			if o := owner[p.Name]; excludedRefs[o] {
				return nil, incompatible("%s's pod selector also matches pod %s of excluded workload %s %s; the outage would take it down too",
					w.ref(), p.Name, o, plan.where())
			}
			affected[p.Name] = true
		}
	}
	for p := range affected {
		plan.Affected = append(plan.Affected, p)
	}
	sort.Strings(plan.Affected)
	return plan, nil
}

// where describes the place for messages: "in zone zone-a (nodes: n1, n2)".
func (p *outagePlan) where() string {
	if p.Field == "node" {
		return "on node " + p.Place
	}
	return fmt.Sprintf("in zone %s (nodes: %s)", p.Place, strings.Join(p.Nodes, ", "))
}

// outageNodes returns the nodes the outage covers: every node labelled with
// the zone, or the one node named.
func (d *Driver) outageNodes(ctx context.Context, plan *outagePlan) ([]string, error) {
	if plan.Field == "node" {
		_, err := d.kube.CoreV1().Nodes().Get(ctx, plan.Place, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, incompatible("no node named %q", plan.Place)
		}
		if err != nil {
			return nil, fmt.Errorf("get node %s: %w", plan.Place, err)
		}
		return []string{plan.Place}, nil
	}
	list, err := d.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var nodes []string
	zones := map[string]bool{}
	for _, n := range list.Items {
		z := n.Labels[catalog.ZoneLabel]
		if z != "" {
			zones[z] = true
		}
		if z == plan.Place {
			nodes = append(nodes, n.Name)
		}
	}
	if len(nodes) == 0 {
		known := make([]string, 0, len(zones))
		for z := range zones {
			known = append(known, z)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return nil, incompatible("no node carries the %s label, so there is no zone to take down", catalog.ZoneLabel)
		}
		return nil, incompatible("no node is in zone %q (%s); zones: %s", plan.Place, catalog.ZoneLabel, strings.Join(known, ", "))
	}
	sort.Strings(nodes)
	return nodes, nil
}

// arenaWorkloads lists the namespace's Deployments, StatefulSets and
// DaemonSets with their pod selectors.
func (d *Driver) arenaWorkloads(ctx context.Context, ns string) ([]outageWorkload, error) {
	var out []outageWorkload
	deps, err := d.kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	for _, w := range deps.Items {
		out = append(out, outageWorkload{Kind: "Deployment", Name: w.Name, selector: w.Spec.Selector})
	}
	sts, err := d.kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
	}
	for _, w := range sts.Items {
		out = append(out, outageWorkload{Kind: "StatefulSet", Name: w.Name, selector: w.Spec.Selector})
	}
	dss, err := d.kube.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
	}
	for _, w := range dss.Items {
		out = append(out, outageWorkload{Kind: "DaemonSet", Name: w.Name, selector: w.Spec.Selector})
	}
	// A workload with no selector to aim a PodChaos by is not one an outage
	// can take down on its own; it is left out rather than aimed at the
	// whole namespace.
	out = slices.DeleteFunc(out, func(w outageWorkload) bool {
		return w.selector == nil || (len(w.selector.MatchLabels) == 0 && len(w.selector.MatchExpressions) == 0)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ref() < out[j].ref() })
	return out, nil
}

// owns reports whether p belongs to w: w's selector matches it, and its
// controller is w — directly for a StatefulSet or DaemonSet, through the
// ReplicaSet named after the pod-template hash for a Deployment. The owner is
// read off the pod, so no ReplicaSet read is needed.
func owns(w outageWorkload, p corev1.Pod) bool {
	sel, err := metav1.LabelSelectorAsSelector(w.selector)
	if err != nil || !sel.Matches(labels.Set(p.Labels)) {
		return false
	}
	ref := metav1.GetControllerOf(&p)
	if ref == nil {
		return false
	}
	switch w.Kind {
	case "Deployment":
		hash := p.Labels["pod-template-hash"]
		return ref.Kind == "ReplicaSet" && hash != "" && ref.Name == w.Name+"-"+hash
	default:
		return ref.Kind == w.Kind && ref.Name == w.Name
	}
}

// excludedWorkloads is the union of what the executor passed on ctx and the
// namespace's own annotation. The executor's list is authoritative; the
// annotation is read as well so the driver never selects an excluded
// workload, whoever calls it.
func (d *Driver) excludedWorkloads(ctx context.Context, ns string) ([]string, error) {
	out := simian.ExcludedWorkloadsFrom(ctx, ns)
	n, err := d.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("read exclusions of %s: %w", ns, err)
	default:
		for _, w := range strings.Split(n.Annotations[simian.ExcludeWorkloadsAnnotation], ",") {
			if w = strings.TrimSpace(w); w != "" && !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	return out, nil
}

// podChaosGVR resolves PodChaos in the group version the manifest names.
func (d *Driver) podChaosGVR(apiVersion string) (schema.GroupVersionResource, error) {
	return d.gvrFor(schema.FromAPIVersionAndKind(apiVersion, "PodChaos"))
}

// applyOutage creates the bundle. Either every member is created or none is
// left: a half-applied outage is a different fault from the one asked for.
func (d *Driver) applyOutage(ctx context.Context, m simian.FaultManifest) (string, error) {
	plan, err := d.planOutage(ctx, m)
	if err != nil {
		return "", fmt.Errorf("chaos-mesh apply %s: %w", m.ResourceKind, err)
	}
	gvr, err := d.podChaosGVR(m.APIVersion)
	if err != nil {
		return "", fmt.Errorf("chaos-mesh apply %s: %w", m.ResourceKind, err)
	}
	bundle := strings.ToLower(ulid.Make().String())
	outageSpec, err := json.Marshal(map[string]any{plan.Field: plan.Place, "action": catalog.OutageAction(m.Spec)})
	if err != nil {
		return "", fmt.Errorf("chaos-mesh apply %s: %w", m.ResourceKind, err)
	}
	var expiry string
	if m.Duration > 0 {
		expiry = time.Now().Add(m.Duration).UTC().Format(time.RFC3339)
	}
	targetLabels, targetAnnotations := m.TargetNamespaceMarks()

	objects := map[string]string{}
	var created []string
	for i, w := range plan.Workloads {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(m.APIVersion)
		obj.SetKind("PodChaos")
		obj.SetName(fmt.Sprintf("%s%s-%d", d.namePrefix, bundle, i))
		obj.SetNamespace(plan.Namespace)
		lbls := map[string]string{
			"simian.chaos/managed":   "true",
			"simian.chaos/fault-uid": m.UID,
			BundleLabel:              bundle,
		}
		for k, v := range targetLabels {
			lbls[k] = v
		}
		obj.SetLabels(lbls)
		annotations := map[string]string{
			outageKindAnnotation: m.ResourceKind,
			outageSpecAnnotation: string(outageSpec),
			bundleSizeAnnotation: strconv.Itoa(len(plan.Workloads)),
			workloadAnnotation:   w.ref(),
		}
		for k, v := range targetAnnotations {
			annotations[k] = v
		}
		if expiry != "" {
			annotations[ExpiryAnnotation] = expiry
		}
		obj.SetAnnotations(annotations)
		if err := unstructured.SetNestedMap(obj.Object, memberSpec(plan, w, m.Duration), "spec"); err != nil {
			return "", fmt.Errorf("chaos-mesh apply %s: set spec: %w", m.ResourceKind, err)
		}
		opts := metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict}
		got, err := d.dyn.Resource(gvr).Namespace(plan.Namespace).Create(ctx, obj, opts)
		if err != nil {
			rollback := d.deleteMembers(context.WithoutCancel(ctx), gvr, plan.Namespace, created)
			return "", errors.Join(
				fmt.Errorf("chaos-mesh apply %s: create PodChaos for %s: %w; the %d member(s) already created were deleted", m.ResourceKind, w.ref(), err, len(created)),
				rollback)
		}
		created = append(created, got.GetName())
		objects[w.ref()] = got.GetName()
	}
	simian.RecordApplyDetail(ctx, "outage", plan.auditDetail(objects))
	return bundleUID(plan.Namespace, bundle, gvr), nil
}

// memberSpec is one member's PodChaos spec: every pod of the workload in the
// place, failed for the duration.
func memberSpec(plan *outagePlan, w outageWorkload, duration time.Duration) map[string]any {
	sel := map[string]any{"namespaces": []any{plan.Namespace}}
	if len(w.selector.MatchLabels) > 0 {
		ls := map[string]any{}
		for k, v := range w.selector.MatchLabels {
			ls[k] = v
		}
		sel["labelSelectors"] = ls
	}
	if len(w.selector.MatchExpressions) > 0 {
		exprs := make([]any, 0, len(w.selector.MatchExpressions))
		for _, e := range w.selector.MatchExpressions {
			values := make([]any, 0, len(e.Values))
			for _, v := range e.Values {
				values = append(values, v)
			}
			exprs = append(exprs, map[string]any{"key": e.Key, "operator": string(e.Operator), "values": values})
		}
		sel["expressionSelectors"] = exprs
	}
	if plan.Field == "node" {
		sel["nodes"] = []any{plan.Place}
	} else {
		sel["nodeSelectors"] = map[string]any{catalog.ZoneLabel: plan.Place}
	}
	spec := map[string]any{
		"action":   catalog.OutageActionPodFailure,
		"mode":     "all",
		"selector": sel,
	}
	if duration > 0 {
		spec["duration"] = duration.String()
	}
	return spec
}

// deleteMembers deletes the named members, NotFound counting as done.
func (d *Driver) deleteMembers(ctx context.Context, gvr schema.GroupVersionResource, ns string, names []string) error {
	var errs []error
	for _, name := range names {
		err := d.dyn.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s/%s: %w", ns, name, err))
		}
	}
	return errors.Join(errs...)
}

// listMembers returns the bundle's members.
func (d *Driver) listMembers(ctx context.Context, gvr schema.GroupVersionResource, ns, id string) ([]unstructured.Unstructured, error) {
	list, err := d.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{BundleLabel: id, "simian.chaos/managed": "true"}).String(),
	})
	if err != nil {
		return nil, err
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	return items, nil
}

// clearBundle deletes every member of a bundle. Idempotent.
func (d *Driver) clearBundle(ctx context.Context, ns, id string, gvr schema.GroupVersionResource) error {
	members, err := d.listMembers(ctx, gvr, ns, id)
	if err != nil {
		return fmt.Errorf("chaos-mesh clear bundle %s/%s: %w", ns, id, err)
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.GetName())
	}
	if err := d.deleteMembers(ctx, gvr, ns, names); err != nil {
		return fmt.Errorf("chaos-mesh clear bundle %s/%s: %w", ns, id, err)
	}
	return nil
}

// bundleState reads a bundle's members and reports whether every one has
// been injected, with a line per workload.
func (d *Driver) bundleState(ctx context.Context, ns, id string, gvr schema.GroupVersionResource) (bool, string, error) {
	members, err := d.listMembers(ctx, gvr, ns, id)
	if err != nil {
		return false, "", err
	}
	if len(members) == 0 {
		return false, "", apierrors.NewNotFound(gvr.GroupResource(), id)
	}
	size, _ := strconv.Atoi(members[0].GetAnnotations()[bundleSizeAnnotation])
	done := size <= len(members)
	parts := make([]string, 0, len(members)+1)
	if !done {
		parts = append(parts, fmt.Sprintf("%d of %d member(s) present", len(members), size))
	}
	for i := range members {
		ok, observed := injectionState(&members[i])
		done = done && ok
		who := members[i].GetAnnotations()[workloadAnnotation]
		if who == "" {
			who = members[i].GetName()
		}
		parts = append(parts, who+": "+observed)
	}
	return done, strings.Join(parts, "; "), nil
}

// outageCatalog returns the outage kinds' catalog entries, offered when
// PodChaos is installed and the driver can read nodes and pods.
func (d *Driver) outageCatalog(preferred string, resources []metav1.APIResource) []simian.CatalogEntry {
	if d.kube == nil || !slices.ContainsFunc(resources, func(r metav1.APIResource) bool { return r.Kind == "PodChaos" }) {
		return nil
	}
	out := make([]simian.CatalogEntry, 0, 2)
	for _, kind := range []string{catalog.ChaosMeshZoneOutage, catalog.ChaosMeshNodeOutage} {
		out = append(out, simian.CatalogEntry{
			Engine:          simian.EngineChaosMesh,
			APIVersion:      preferred,
			ResourceKind:    kind,
			BlastRadiusTier: catalog.Classify(simian.EngineChaosMesh, kind),
			Description:     kind + " (composite: one PodChaos pod-failure per arena workload there)",
			SpecTemplate:    specTemplates[kind],
			EfficacyGate:    catalog.EfficacyGate(simian.EngineChaosMesh, kind),
		})
	}
	return out
}

// liveBundle is one bundle being gathered by ListLive.
type liveBundle struct {
	af   simian.ActiveFault
	seen bool
}

// bundleFault turns a live member into the ActiveFault for its whole bundle,
// or merges it into the one already started.
func bundleFault(b *liveBundle, obj *unstructured.Unstructured, ns string, gvr schema.GroupVersionResource, deadline time.Time) {
	created := obj.GetCreationTimestamp().UTC()
	if !b.seen {
		b.seen = true
		ann := obj.GetAnnotations()
		var spec map[string]any
		_ = json.Unmarshal([]byte(ann[outageSpecAnnotation]), &spec)
		kind := ann[outageKindAnnotation]
		if kind == "" {
			kind = "PodChaos"
		}
		faultUID := obj.GetLabels()["simian.chaos/fault-uid"]
		b.af = simian.ActiveFault{
			FaultUID:  faultUID,
			EngineUID: bundleUID(ns, obj.GetLabels()[BundleLabel], gvr),
			Manifest: simian.FaultManifest{
				UID:          faultUID,
				Engine:       simian.EngineChaosMesh,
				APIVersion:   obj.GetAPIVersion(),
				ResourceKind: kind,
				Spec:         spec,
				Targets:      simian.ObjectTargets(obj.GetLabels(), ann, ns),
			},
			AppliedAt: created,
			Deadline:  deadline,
		}
		return
	}
	if created.Before(b.af.AppliedAt) {
		b.af.AppliedAt = created
	}
	if deadline.After(b.af.Deadline) {
		b.af.Deadline = deadline
	}
}
