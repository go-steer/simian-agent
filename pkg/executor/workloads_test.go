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

package executor

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func deployment(ns, name string, sel *metav1.LabelSelector) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: sel},
	}
}

// #144: `simian chaos --workload paymentservice` sends a target with a name
// and no labels. The gate has to see the workload's pods, not none.
func TestANamedTargetIsGatedOnItsWorkloadsPods(t *testing.T) {
	client := fake.NewClientset(deployment("online-boutique", "paymentservice",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "paymentservice"}}))
	prober := &fakeProber{}
	exec, _, auditor, _ := newProbedExecutor(t, prober, WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: client}))

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{sotProbe("reachable")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prober.seen) == 0 {
		t.Fatal("no probe ran")
	}
	if got := prober.seen[0].Labels; !maps.Equal(got, map[string]string{"app": "paymentservice"}) {
		t.Errorf("probe target labels = %v, want the Deployment's selector", got)
	}
	validated, _ := auditor.FindEvent(audit.EventExecutorValidated)
	if got, _ := validated.Payload["target_labels_from_workload"].([]string); !slices.Equal(got, []string{"online-boutique/paymentservice"}) {
		t.Errorf("validated payload = %v, want the resolution recorded", validated.Payload)
	}
}

func TestLabelsTheManifestBringsAreLeftAlone(t *testing.T) {
	client := fake.NewClientset(deployment("online-boutique", "paymentservice",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "paymentservice"}}))
	prober := &fakeProber{}
	exec, _, _, _ := newProbedExecutor(t, prober, WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: client}))

	m := goodManifest()
	m.Targets[0].Labels = map[string]string{"tier": "payments"}
	m.Probes = []simian.ProbeSpec{sotProbe("reachable")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := prober.seen[0].Labels; !maps.Equal(got, map[string]string{"tier": "payments"}) {
		t.Errorf("probe target labels = %v, want the manifest's own", got)
	}
}

// When the spec says which pods it means, a target whose workload cannot be
// read is recorded and the fault goes ahead on the spec's own selector.
func TestAWorkloadThatCannotBeResolvedIsRecordedNotFatal(t *testing.T) {
	prober := &fakeProber{}
	exec, _, auditor, _ := newProbedExecutor(t, prober, WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: fake.NewClientset()}))

	m := goodManifest()
	m.Spec["selector"] = map[string]any{"labelSelectors": map[string]any{"app": "paymentservice"}}
	m.Probes = []simian.ProbeSpec{sotProbe("reachable")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	validated, _ := auditor.FindEvent(audit.EventExecutorValidated)
	got, _ := validated.Payload["target_labels_unresolved"].([]string)
	if len(got) != 1 || !strings.Contains(got[0], "no Deployment, StatefulSet or DaemonSet") {
		t.Errorf("target_labels_unresolved = %v", validated.Payload["target_labels_unresolved"])
	}
}

func TestKubernetesWorkloadSelectors(t *testing.T) {
	client := fake.NewClientset(
		deployment("ns", "web", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}),
		deployment("ns", "fancy", &metav1.LabelSelector{
			MatchLabels:      map[string]string{"app": "fancy"},
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}}},
		}),
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db"},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
		},
	)
	ws := KubernetesWorkloadSelectors{Client: client}
	for _, tc := range []struct {
		kind, name string
		want       map[string]string
		wantErr    string
	}{
		{"", "web", map[string]string{"app": "web"}, ""},
		{"", "db", map[string]string{"app": "db"}, ""},
		{"StatefulSet", "db", map[string]string{"app": "db"}, ""},
		{"Deployment", "db", nil, "no Deployment"},
		{"", "fancy", nil, "by expression"},
		{"CronJob", "web", nil, "no pod selector"},
	} {
		got, err := ws.PodLabels(context.Background(), "ns", tc.kind, tc.name)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s/%s: err = %v, want %q", tc.kind, tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !maps.Equal(got, tc.want) {
			t.Errorf("%s/%s = %v, %v; want %v", tc.kind, tc.name, got, err, tc.want)
		}
	}
}

// Getting-started validation, 2026-10-06: `simian chaos --workload
// productcatalogservice --kind PodChaos --spec '{"action":"pod-kill","mode":"one"}'`
// resolved the target's labels and left the selector naming only the
// namespace, and Chaos Mesh killed a frontend pod.
func TestASelectorThatNamesNoPodsIsNarrowedToTheTarget(t *testing.T) {
	client := fake.NewClientset(deployment("online-boutique", "productcatalogservice",
		&metav1.LabelSelector{MatchLabels: map[string]string{"app": "productcatalogservice"}}))
	prober := &fakeProber{}
	exec, driver, auditor, _ := newProbedExecutor(t, prober, WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: client}))

	m := goodManifest()
	m.ResourceKind = "PodChaos"
	m.Spec = map[string]any{"action": "pod-kill", "mode": "one"}
	m.Targets = []simian.TargetRef{{Namespace: "online-boutique", Name: "productcatalogservice"}}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	applied := driver.AppliedCopy()
	sel, _ := applied[len(applied)-1].Spec["selector"].(map[string]any)
	ls, _ := sel["labelSelectors"].(map[string]any)
	if ls["app"] != "productcatalogservice" {
		t.Fatalf("the driver got selector %v, want it narrowed to app=productcatalogservice", sel)
	}
	validated, _ := auditor.FindEvent(audit.EventExecutorValidated)
	if got, _ := validated.Payload["selector_labels_from_targets"].(map[string]string); got["app"] != "productcatalogservice" {
		t.Errorf("validated payload = %v, want the narrowing recorded", validated.Payload)
	}
}

func TestASelectorThatNamesNoPodsIsRefusedWhenItsTargetCannotBeResolved(t *testing.T) {
	prober := &fakeProber{}
	exec, driver, _, _ := newProbedExecutor(t, prober, WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: fake.NewClientset()}))
	m := goodManifest() // NetworkChaos, selector-less, targets paymentservice by name
	before := len(driver.AppliedCopy())
	_, err := exec.Apply(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "every pod in the namespace") {
		t.Fatalf("err = %v, want a refusal that names the namespace-wide reach", err)
	}
	if len(driver.AppliedCopy()) != before {
		t.Error("the driver was called")
	}
}

// A fault aimed at a namespace and no workload in it is a legitimate choice —
// a random pod — but not where it would reach excluded workloads.
func TestANamespaceWideSelectorIsRefusedWhereThereAreExclusions(t *testing.T) {
	m := goodManifest()
	m.ResourceKind = "PodChaos"
	m.Spec = map[string]any{"action": "pod-kill", "mode": "one"}
	m.Targets = []simian.TargetRef{{Namespace: "online-boutique"}}

	open := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}},
		lease.NewRegistry("h"), &testutil.FakeAuditor{}, &StaticEligibility{Eligible: map[string]bool{"online-boutique": true}})
	if _, err := open.Apply(context.Background(), m); err != nil {
		t.Fatalf("no exclusions: Apply: %v", err)
	}

	guarded := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}},
		lease.NewRegistry("h"), &testutil.FakeAuditor{}, &StaticEligibility{Eligible: map[string]bool{"online-boutique": true},
			Exclusions: map[string][]string{"online-boutique": {"loadgenerator"}}})
	_, err := guarded.Apply(context.Background(), m)
	if ee := asExecutorError(t, err); ee.Reason != simian.ReasonWorkloadExcluded || !strings.Contains(err.Error(), "loadgenerator") {
		t.Fatalf("err = %v, want workload-excluded naming loadgenerator", err)
	}
}

// #198: a NetworkChaos partition's far side, spec.target.selector, naming
// only the namespace cuts the caller off from every pod there — the excluded
// load generator included.
func TestASecondarySelectorThatReachesExcludedWorkloadsIsRefused(t *testing.T) {
	partition := func(target map[string]any) simian.FaultManifest {
		m := goodManifest()
		m.Spec = map[string]any{"action": "partition", "direction": "to",
			"selector": map[string]any{"labelSelectors": map[string]any{"app": "edge"}},
			"target":   map[string]any{"mode": "all", "selector": target}}
		m.Targets = []simian.TargetRef{{Namespace: "online-boutique", Name: "edge", Labels: map[string]string{"app": "edge"}}}
		return m
	}
	executor := func(excl map[string][]string) *Executor {
		return New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}},
			lease.NewRegistry("h"), &testutil.FakeAuditor{}, &StaticEligibility{Eligible: map[string]bool{"online-boutique": true}, Exclusions: excl})
	}
	guarded := executor(map[string][]string{"online-boutique": {"loadgenerator"}})

	_, err := guarded.Apply(context.Background(), partition(map[string]any{"namespaces": []any{"online-boutique"}}))
	if ee := asExecutorError(t, err); ee.Reason != simian.ReasonWorkloadExcluded || !strings.Contains(err.Error(), "spec.target.selector") {
		t.Fatalf("namespace-wide far side: err = %v, want workload-excluded naming spec.target.selector", err)
	}
	if _, err := guarded.Apply(context.Background(), partition(map[string]any{"labelSelectors": map[string]any{"app": "upstream"}})); err != nil {
		t.Errorf("a far side naming its pods was refused: %v", err)
	}
	if _, err := executor(nil).Apply(context.Background(), partition(map[string]any{"namespaces": []any{"online-boutique"}})); err != nil {
		t.Errorf("a namespace-wide far side with nothing excluded was refused: %v", err)
	}
}
