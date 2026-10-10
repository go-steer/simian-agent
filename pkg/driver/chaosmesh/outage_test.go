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
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"
)

var (
	_ simian.TargetChecker      = (*Driver)(nil)
	_ simian.LiveFaultLister    = (*Driver)(nil)
	_ simian.OrphanReaper       = (*Driver)(nil)
	_ simian.InjectionConfirmer = (*Driver)(nil)
)

var podChaosGVR = schema.GroupVersionResource{Group: APIGroup, Version: "v1alpha1", Resource: "podchaos"}

const arena = "shop"

func node(name, zone string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if zone != "" {
		n.Labels = map[string]string{catalog.ZoneLabel: zone}
	}
	return n
}

func deployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: arena},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
	}
}

func statefulSet(name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: arena},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
	}
}

// deployPod is a pod of Deployment app, owned through its ReplicaSet.
func deployPod(app, name, nodeName string) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: arena,
			Labels:          map[string]string{"app": app, "pod-template-hash": "h1"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: app + "-h1", Controller: &ctrl}},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func stsPod(app, name, nodeName string) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: arena,
			Labels:          map[string]string{"app": app},
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: app, Controller: &ctrl}},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// shop is an arena across two zones:
//
//	zone-a (a1, a2): web-1, web-2, api-1, loadgen-1
//	zone-b (b1):     web-3, db-0
//
// api is entirely in zone-a; loadgen is excluded by the namespace annotation.
func shop() []k8sruntime.Object {
	return []k8sruntime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: arena,
			Annotations: map[string]string{simian.ExcludeWorkloadsAnnotation: "loadgen"}}},
		node("a1", "zone-a"), node("a2", "zone-a"), node("b1", "zone-b"),
		deployment("web"), deployment("api"), deployment("loadgen"), statefulSet("db"),
		deployPod("web", "web-1", "a1"), deployPod("web", "web-2", "a2"), deployPod("web", "web-3", "b1"),
		deployPod("api", "api-1", "a1"),
		deployPod("loadgen", "loadgen-1", "a2"),
		stsPod("db", "db-0", "b1"),
	}
}

func newOutageDriver(t *testing.T, objs ...k8sruntime.Object) (*Driver, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{
		Resources: []*metav1.APIResourceList{{
			GroupVersion: APIGroup + "/v1alpha1",
			APIResources: []metav1.APIResource{
				{Name: "podchaos", SingularName: "podchaos", Namespaced: true, Kind: "PodChaos"},
			},
		}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		k8sruntime.NewScheme(),
		map[schema.GroupVersionResource]string{podChaosGVR: "PodChaosList"},
	)
	d := (&Driver{
		dyn:        dyn,
		disco:      disco,
		mapper:     restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscovery{disco}),
		namePrefix: "simian-",
	}).WithKubernetes(kubefake.NewClientset(objs...))
	return d, dyn
}

func outage(kind string, spec map[string]any, targets ...simian.TargetRef) simian.FaultManifest {
	if len(targets) == 0 {
		targets = []simian.TargetRef{{Namespace: arena}}
	}
	return simian.FaultManifest{
		UID: "f-outage", Engine: simian.EngineChaosMesh, APIVersion: APIGroup + "/v1alpha1",
		ResourceKind: kind, Spec: spec, Targets: targets, Duration: 2 * time.Minute,
	}
}

func members(t *testing.T, d *Driver) []unstructured.Unstructured {
	t.Helper()
	list, err := d.dyn.Resource(podChaosGVR).Namespace(arena).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list podchaos: %v", err)
	}
	items := list.Items
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return strings.Compare(a.GetAnnotations()[workloadAnnotation], b.GetAnnotations()[workloadAnnotation])
	})
	return items
}

// The outage is a PodChaos per non-excluded workload with pods in the zone,
// each selecting that workload's pods and only those in the zone.
func TestZoneOutageCreatesOnePodChaosPerWorkloadInTheZone(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	ctx, details := simian.WithApplyDetails(context.Background())

	uid, err := d.Apply(ctx, outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(uid, bundlePrefix+arena+"/") {
		t.Errorf("engine UID %q does not name a bundle in %s", uid, arena)
	}

	got := members(t, d)
	if len(got) != 2 {
		t.Fatalf("created %d PodChaos, want 2 (api, web; not loadgen, which is excluded, nor db, which is not in zone-a)", len(got))
	}
	bundle := got[0].GetLabels()[BundleLabel]
	for i, want := range []string{"Deployment/api", "Deployment/web"} {
		obj := got[i]
		if w := obj.GetAnnotations()[workloadAnnotation]; w != want {
			t.Fatalf("member %d is for %s, want %s", i, w, want)
		}
		if obj.GetLabels()[BundleLabel] != bundle || bundle == "" {
			t.Errorf("%s: bundle label %q, want the shared %q", want, obj.GetLabels()[BundleLabel], bundle)
		}
		for k, v := range map[string]string{
			"simian.chaos/managed": "true", "simian.chaos/fault-uid": "f-outage", simian.TargetNamespaceLabel: arena,
		} {
			if obj.GetLabels()[k] != v {
				t.Errorf("%s: label %s = %q, want %q", want, k, obj.GetLabels()[k], v)
			}
		}
		if _, ok := deadlineOf(&obj); !ok {
			t.Errorf("%s: no deadline for the reaper", want)
		}
		spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
		app := strings.TrimPrefix(want, "Deployment/")
		wantSpec := map[string]any{
			"action": "pod-failure", "mode": "all", "duration": "2m0s",
			"selector": map[string]any{
				"namespaces":     []any{arena},
				"labelSelectors": map[string]any{"app": app},
				"nodeSelectors":  map[string]any{catalog.ZoneLabel: "zone-a"},
			},
		}
		if !reflect.DeepEqual(spec, wantSpec) {
			t.Errorf("%s spec =\n%v\nwant\n%v", want, spec, wantSpec)
		}
	}

	detail, _ := details.All()["outage"].(map[string]any)
	if detail == nil {
		t.Fatal("no outage detail recorded for driver.applied")
	}
	if detail["zone"] != "zone-a" || !reflect.DeepEqual(detail["nodes"], []string{"a1", "a2"}) {
		t.Errorf("detail names zone %v nodes %v", detail["zone"], detail["nodes"])
	}
	if detail["pods_affected"] != 3 || !reflect.DeepEqual(detail["pods"], []string{"api-1", "web-1", "web-2"}) {
		t.Errorf("pods affected = %v %v, want 3 [api-1 web-1 web-2]", detail["pods_affected"], detail["pods"])
	}
	if !reflect.DeepEqual(detail["excluded_skipped"], []string{"Deployment/loadgen"}) {
		t.Errorf("excluded_skipped = %v", detail["excluded_skipped"])
	}
	if !reflect.DeepEqual(detail["fully_down"], []string{"Deployment/api"}) {
		t.Errorf("fully_down = %v, want api: all its pods are in zone-a", detail["fully_down"])
	}
	workloads, _ := detail["workloads"].([]any)
	if len(workloads) != 2 {
		t.Fatalf("workloads = %v", workloads)
	}
	web, _ := workloads[1].(map[string]any)
	if web["name"] != "web" || web["pods_total"] != 3 || web["fully_down"] != false ||
		!reflect.DeepEqual(web["pods"], []string{"web-1", "web-2"}) || web["object"] == "" {
		t.Errorf("web entry = %v", web)
	}
}

func TestNodeOutageSelectsThePodsOnTheNode(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	if _, err := d.Apply(context.Background(), outage(catalog.ChaosMeshNodeOutage, map[string]any{"node": "b1"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := members(t, d)
	if len(got) != 2 {
		t.Fatalf("created %d PodChaos, want 2 (db, web)", len(got))
	}
	for _, obj := range got {
		sel, _, _ := unstructured.NestedMap(obj.Object, "spec", "selector")
		if !reflect.DeepEqual(sel["nodes"], []any{"b1"}) || sel["nodeSelectors"] != nil {
			t.Errorf("%s selector = %v, want nodes [b1]", obj.GetAnnotations()[workloadAnnotation], sel)
		}
	}
}

// The executor's exclusions are honoured even where the namespace's own
// annotation does not list them — a static allowlist, say.
func TestExclusionsFromTheExecutorAreSkipped(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	ctx := simian.WithExcludedWorkloads(context.Background(), map[string][]string{arena: {"api"}})
	if _, err := d.Apply(ctx, outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := members(t, d)
	if len(got) != 1 || got[0].GetAnnotations()[workloadAnnotation] != "Deployment/web" {
		t.Fatalf("members = %d, want only web", len(got))
	}
}

func TestNamedTargetsLimitTheOutageToThoseWorkloads(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	m := outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}, simian.TargetRef{Namespace: arena, Name: "web"})
	if _, err := d.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := members(t, d); len(got) != 1 || got[0].GetAnnotations()[workloadAnnotation] != "Deployment/web" {
		t.Fatalf("members = %d, want only web", len(got))
	}
}

func TestAnOutageWithNothingToTakeDownIsRefused(t *testing.T) {
	onlyExcluded := append(shop(), node("c1", "zone-c"), deployPod("loadgen", "loadgen-2", "c1"))
	for _, tc := range []struct {
		name string
		objs []k8sruntime.Object
		kind string
		spec map[string]any
		says string
	}{
		{"a zone no node is in", shop(), catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-x"}, "zones: zone-a, zone-b"},
		{"a cluster without zone labels", []k8sruntime.Object{node("n1", ""), deployment("web"), deployPod("web", "web-1", "n1")},
			catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}, "no node carries"},
		{"a zone with only excluded pods", onlyExcluded, catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-c"}, "excluded, and skipped: Deployment/loadgen"},
		{"a node that does not exist", shop(), catalog.ChaosMeshNodeOutage, map[string]any{"node": "zz"}, `no node named "zz"`},
		{"a node with no arena pods", append(shop(), node("c2", "zone-c")), catalog.ChaosMeshNodeOutage, map[string]any{"node": "c2"}, "no workload of arena shop has pods on node c2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newOutageDriver(t, tc.objs...)
			m := outage(tc.kind, tc.spec)
			err := d.CheckTargets(context.Background(), m)
			if !errors.Is(err, simian.ErrTargetIncompatible) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("CheckTargets = %v, want target-incompatible saying %q", err, tc.says)
			}
			if _, err := d.Apply(context.Background(), m); !errors.Is(err, simian.ErrTargetIncompatible) {
				t.Errorf("Apply = %v, want it refused the same way", err)
			}
			if got := members(t, d); len(got) != 0 {
				t.Errorf("%d PodChaos created", len(got))
			}
		})
	}
}

// A selector wider than its workload would take an excluded pod down with
// it, so the outage is refused rather than built.
func TestAnOutageWhoseSelectorReachesAnExcludedPodIsRefused(t *testing.T) {
	objs := shop()
	ctrl := true
	sneaky := deployPod("loadgen", "loadgen-web", "a1")
	sneaky.Labels["app"] = "web" // carries web's label, owned by loadgen
	sneaky.Labels["pod-template-hash"] = "h1"
	sneaky.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "loadgen-h1", Controller: &ctrl}}
	// loadgen must own it for the check to see it: widen loadgen's selector.
	for i, o := range objs {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == "loadgen" {
			dep.Spec.Selector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"loadgen", "web"}},
			}}
			objs[i] = dep
		}
	}
	d, _ := newOutageDriver(t, append(objs, sneaky)...)
	err := d.CheckTargets(context.Background(), outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if !errors.Is(err, simian.ErrTargetIncompatible) || !strings.Contains(err.Error(), "loadgen-web") {
		t.Fatalf("CheckTargets = %v, want a refusal naming loadgen-web", err)
	}
}

func TestAMalformedOutageSpecIsRefused(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	for _, spec := range []map[string]any{
		{},
		{"zone": "zone-a", "action": "pod-kill"},
		{"zone": "zone-a", "selector": map[string]any{"namespaces": []any{"kube-system"}}},
		{"zone": "zone-a", "mode": "all"},
	} {
		if _, err := d.Apply(context.Background(), outage(catalog.ChaosMeshZoneOutage, spec)); err == nil {
			t.Errorf("spec %v was applied", spec)
		}
	}
	two := outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"},
		simian.TargetRef{Namespace: arena}, simian.TargetRef{Namespace: "other"})
	if _, err := d.Apply(context.Background(), two); err == nil {
		t.Error("an outage across two namespaces was applied")
	}
	if got := members(t, d); len(got) != 0 {
		t.Errorf("%d PodChaos created", len(got))
	}
}

// No half-applied outage: when one member cannot be created, the ones that
// were are deleted.
func TestAFailedMemberCreateRollsBackTheOthers(t *testing.T) {
	d, dyn := newOutageDriver(t, shop()...)
	creates := 0
	dyn.PrependReactor("create", "podchaos", func(clienttesting.Action) (bool, k8sruntime.Object, error) {
		creates++
		if creates == 2 {
			return true, nil, fmt.Errorf("admission webhook said no")
		}
		return false, nil, nil
	})
	_, err := d.Apply(context.Background(), outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if err == nil || !strings.Contains(err.Error(), "admission webhook said no") || !strings.Contains(err.Error(), "1 member(s) already created were deleted") {
		t.Fatalf("Apply = %v", err)
	}
	if got := members(t, d); len(got) != 0 {
		t.Errorf("%d PodChaos left behind after a failed apply", len(got))
	}
}

func TestClearDeletesEveryMemberAndOnlyThem(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	uid, err := d.Apply(context.Background(), outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	other, err := d.Apply(context.Background(), outage(catalog.ChaosMeshNodeOutage, map[string]any{"node": "b1"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got := members(t, d)
	if len(got) != 2 {
		t.Fatalf("%d PodChaos left, want the other outage's 2", len(got))
	}
	for _, obj := range got {
		if bundleUID(arena, obj.GetLabels()[BundleLabel], podChaosGVR) != other {
			t.Errorf("%s survived but belongs to the cleared bundle", obj.GetName())
		}
	}
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Errorf("second Clear: %v; must be idempotent", err)
	}
}

func setStatus(t *testing.T, d *Driver, obj unstructured.Unstructured, status map[string]any) {
	t.Helper()
	obj.Object["status"] = status
	if _, err := d.dyn.Resource(podChaosGVR).Namespace(arena).Update(context.Background(), &obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
}

func TestConfirmRequiresEveryMemberInjected(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	d.confirmTimeout, d.confirmInterval = 50*time.Millisecond, 10*time.Millisecond
	uid, err := d.Apply(context.Background(), outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := members(t, d)
	injected := map[string]any{"conditions": cond("Selected", "True", "AllInjected", "True")}
	setStatus(t, d, got[0], injected)

	observed, err := d.ConfirmInjected(context.Background(), uid)
	if err == nil {
		t.Fatalf("confirmed with one member of two injected: %s", observed)
	}
	if !strings.Contains(observed, "Deployment/api: AllInjected=True") || !strings.Contains(observed, "Deployment/web: no status conditions yet") {
		t.Errorf("observed = %q, want a line per workload", observed)
	}

	setStatus(t, d, got[1], injected)
	if observed, err := d.ConfirmInjected(context.Background(), uid); err != nil {
		t.Errorf("not confirmed with every member injected: %v (%s)", err, observed)
	}

	// A member gone missing is not a complete outage.
	if err := d.dyn.Resource(podChaosGVR).Namespace(arena).Delete(context.Background(), got[1].GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if observed, err := d.ConfirmInjected(context.Background(), uid); err == nil || !strings.Contains(observed, "1 of 2 member(s) present") {
		t.Errorf("confirmed with a member missing: %v (%s)", err, observed)
	}
}

// A restarted controller adopts the outage as one fault, of the kind it was
// applied as — not as two PodChaos it would lease and clear separately.
func TestListLiveReportsABundleAsOneFault(t *testing.T) {
	d, _ := newOutageDriver(t, shop()...)
	uid, err := d.Apply(context.Background(), outage(catalog.ChaosMeshZoneOutage, map[string]any{"zone": "zone-a"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	live, err := d.ListLive(context.Background(), []string{arena}, time.Now())
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("ListLive found %d faults, want 1: %+v", len(live), live)
	}
	af := live[0]
	if af.FaultUID != "f-outage" || af.EngineUID != uid {
		t.Errorf("fault %q engine %q, want f-outage %q", af.FaultUID, af.EngineUID, uid)
	}
	if af.Manifest.ResourceKind != catalog.ChaosMeshZoneOutage || af.Manifest.Spec["zone"] != "zone-a" {
		t.Errorf("adopted as %s %v", af.Manifest.ResourceKind, af.Manifest.Spec)
	}
	if !reflect.DeepEqual(af.Manifest.Targets, []simian.TargetRef{{Namespace: arena}}) {
		t.Errorf("targets = %v", af.Manifest.Targets)
	}
	if until := time.Until(af.Deadline); until < time.Minute || until > 2*time.Minute+time.Second {
		t.Errorf("deadline in %s, want about 2m", until)
	}

	// After its deadline it is not live, and the reaper takes it, reporting
	// the bundle once.
	later := time.Now().Add(2*time.Minute + reapGrace + time.Second)
	if live, _ := d.ListLive(context.Background(), []string{arena}, later); len(live) != 0 {
		t.Errorf("an expired outage is still live: %v", live)
	}
	cleared, err := d.ReapExpired(context.Background(), []string{arena}, later)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if !slices.Equal(cleared, []string{uid}) {
		t.Errorf("reaped %v, want the bundle once: %s", cleared, uid)
	}
	if got := members(t, d); len(got) != 0 {
		t.Errorf("%d members left after reaping", len(got))
	}
}

func TestOutageKindsAreInTheCatalogOnlyWithAKubernetesClient(t *testing.T) {
	d, _ := newOutageDriver(t)
	cat, err := d.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range cat {
		kinds = append(kinds, e.ResourceKind)
		if catalog.IsOutageKind(e.Engine, e.ResourceKind) && (e.BlastRadiusTier != simian.TierNamespace || e.SpecTemplate == "") {
			t.Errorf("%s: tier %s, template %q", e.ResourceKind, e.BlastRadiusTier, e.SpecTemplate)
		}
	}
	if !slices.Equal(kinds, []string{"PodChaos", "ZoneOutage", "NodeOutage"}) {
		t.Errorf("catalog kinds = %v", kinds)
	}
	d.kube = nil
	cat, _ = d.Catalog(context.Background())
	if len(cat) != 1 {
		t.Errorf("without a Kubernetes client the catalog has %d entries, want PodChaos alone", len(cat))
	}
}
