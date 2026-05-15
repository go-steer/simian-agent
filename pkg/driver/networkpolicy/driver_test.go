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

package networkpolicy

import (
	"context"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func sampleManifest() simian.FaultManifest {
	return simian.FaultManifest{
		UID:          "f-test",
		Source:       simian.SourceAutonomous,
		Engine:       simian.EngineNetworkPolicy,
		APIVersion:   APIVersion,
		ResourceKind: Kind,
		Spec: map[string]any{
			"labelSelectors": map[string]any{"app": "frontend"},
			"directions":     []any{"ingress", "egress"},
		},
		Targets:  []simian.TargetRef{{Namespace: "boutique-m3", Name: "frontend"}},
		Duration: 30 * time.Second,
	}
}

func TestEngine(t *testing.T) {
	d := New(fake.NewSimpleClientset(), "")
	if got := d.Engine(); got != simian.EngineNetworkPolicy {
		t.Errorf("Engine()=%q, want %q", got, simian.EngineNetworkPolicy)
	}
}

func TestApplyAndClearRoundTrip(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")

	uid, err := d.Apply(context.Background(), sampleManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(uid, "boutique-m3/simian-np-") {
		t.Errorf("engineUID prefix unexpected: %q", uid)
	}

	// The created NetworkPolicy should deny all ingress + egress to the
	// labeled pods. Verify the in-cluster shape.
	nps, err := cs.NetworkingV1().NetworkPolicies("boutique-m3").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(nps.Items) != 1 {
		t.Fatalf("expected 1 NetworkPolicy, got %d", len(nps.Items))
	}
	np := nps.Items[0]
	if got := np.Spec.PodSelector.MatchLabels["app"]; got != "frontend" {
		t.Errorf("podSelector.matchLabels[app]=%q, want frontend", got)
	}
	if !containsType(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) ||
		!containsType(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Errorf("policyTypes should include both Ingress and Egress; got %v", np.Spec.PolicyTypes)
	}
	if np.Spec.Ingress == nil || len(np.Spec.Ingress) != 0 {
		t.Errorf("Ingress should be a non-nil empty slice (deny all); got %#v", np.Spec.Ingress)
	}
	if np.Spec.Egress == nil || len(np.Spec.Egress) != 0 {
		t.Errorf("Egress should be a non-nil empty slice (deny all); got %#v", np.Spec.Egress)
	}
	if got := np.Labels["simian.chaos/managed"]; got != "true" {
		t.Errorf("missing simian.chaos/managed label: %v", np.Labels)
	}

	// Clear should remove the policy and be idempotent.
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Errorf("Clear (idempotent): %v", err)
	}
	nps, _ = cs.NetworkingV1().NetworkPolicies("boutique-m3").List(context.Background(), metav1.ListOptions{})
	if len(nps.Items) != 0 {
		t.Errorf("expected 0 policies after Clear, got %d", len(nps.Items))
	}
}

func TestApplyDefaultsToBothDirections(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")
	m := sampleManifest()
	delete(m.Spec, "directions") // omit — should default to both

	if _, err := d.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nps, _ := cs.NetworkingV1().NetworkPolicies("boutique-m3").List(context.Background(), metav1.ListOptions{})
	if len(nps.Items) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(nps.Items))
	}
	np := nps.Items[0]
	if !containsType(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) ||
		!containsType(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Errorf("default directions should be both; got %v", np.Spec.PolicyTypes)
	}
}

func TestApplyIngressOnly(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")
	m := sampleManifest()
	m.Spec["directions"] = []any{"ingress"}

	if _, err := d.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	nps, _ := cs.NetworkingV1().NetworkPolicies("boutique-m3").List(context.Background(), metav1.ListOptions{})
	np := nps.Items[0]
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes should be [Ingress] only; got %v", np.Spec.PolicyTypes)
	}
	if np.Spec.Egress != nil {
		t.Errorf("Egress should be nil when not in policyTypes; got %#v", np.Spec.Egress)
	}
}

func TestApplyRejectsMissingLabelSelectors(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")
	m := sampleManifest()
	delete(m.Spec, "labelSelectors")

	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject missing labelSelectors")
	}
}

func TestApplyRejectsInvalidDirection(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")
	m := sampleManifest()
	m.Spec["directions"] = []any{"sideways"}

	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject invalid direction string")
	}
}

func TestApplyRejectsMissingNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")
	m := sampleManifest()
	m.Targets[0].Namespace = ""

	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject empty target namespace")
	}
}

func TestCatalog(t *testing.T) {
	d := New(fake.NewSimpleClientset(), "")
	cat, err := d.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(cat) != 1 {
		t.Fatalf("expected 1 catalog entry, got %d", len(cat))
	}
	e := cat[0]
	if e.Engine != simian.EngineNetworkPolicy {
		t.Errorf("Engine=%q, want %q", e.Engine, simian.EngineNetworkPolicy)
	}
	if e.ResourceKind != Kind {
		t.Errorf("ResourceKind=%q, want %q", e.ResourceKind, Kind)
	}
	if e.BlastRadiusTier != simian.TierNamespace {
		t.Errorf("tier=%q, want %q", e.BlastRadiusTier, simian.TierNamespace)
	}
	if e.SpecTemplate == "" {
		t.Error("SpecTemplate should not be empty — planner won't have shape guidance for the LLM")
	}
	if !strings.Contains(e.SpecTemplate, "labelSelectors") {
		t.Errorf("SpecTemplate should mention labelSelectors:\n%s", e.SpecTemplate)
	}
	if !strings.Contains(e.SpecTemplate, "directions") {
		t.Errorf("SpecTemplate should mention directions:\n%s", e.SpecTemplate)
	}
}

func TestDecodeEngineUIDInvalid(t *testing.T) {
	if _, _, err := decodeEngineUID("nopens"); err == nil {
		t.Error("decodeEngineUID should reject string without /")
	}
}

func containsType(types []networkingv1.PolicyType, want networkingv1.PolicyType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}
