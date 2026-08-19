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
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

// The expiry stamp is the only record of a partition's deadline that survives
// the process. If Apply stops writing it, nothing fails until a Simian is
// killed mid-fault — at which point the partition is permanent.
func TestApplyStampsTheDeadlineOnThePolicy(t *testing.T) {
	cs := fake.NewSimpleClientset()
	applied := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d := New(cs, "")
	d.Now = func() time.Time { return applied }

	m := sampleManifest() // Duration: 30s
	if _, err := d.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	np := onlyPolicy(t, cs, "boutique-m3")
	got, ok := np.Annotations[ExpiryAnnotation]
	if !ok {
		t.Fatalf("policy has no %s annotation; annotations=%v", ExpiryAnnotation, np.Annotations)
	}
	if want := "2026-03-01T12:00:30Z"; got != want {
		t.Errorf("%s = %q, want %q", ExpiryAnnotation, got, want)
	}
	if np.Labels[ManagedLabel] != "true" {
		t.Errorf("policy must carry %s=true or the reaper cannot find it; labels=%v", ManagedLabel, np.Labels)
	}
}

// A manifest with no duration must not be stamped. Stamping now+0 would make
// the very next sweep delete a fault that just started.
func TestApplyDoesNotStampAnAlreadyPassedDeadline(t *testing.T) {
	cs := fake.NewSimpleClientset()
	d := New(cs, "")

	m := sampleManifest()
	m.Duration = 0
	if _, err := d.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if v, ok := onlyPolicy(t, cs, "boutique-m3").Annotations[ExpiryAnnotation]; ok {
		t.Errorf("durationless fault was stamped %q; the next sweep would clear it immediately", v)
	}
}

func TestReapExpiredClearsOnlyWhatHasActuallyExpired(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	policy := func(ns, name string, ann map[string]string, labels map[string]string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Annotations: ann, Labels: labels,
		}}
	}
	ours := map[string]string{ManagedLabel: "true"}
	at := func(t time.Time) map[string]string {
		return map[string]string{ExpiryAnnotation: t.Format(time.RFC3339)}
	}

	cs := fake.NewSimpleClientset(
		policy("arena-a", "expired", at(now.Add(-time.Minute)), ours),
		policy("arena-a", "still-running", at(now.Add(time.Minute)), ours),
		// Exactly at the deadline is not yet past it.
		policy("arena-a", "on-the-boundary", at(now), ours),
		// Ours but unstamped: an older Simian, or something wearing our
		// label. We cannot prove it expired, so we leave it.
		policy("arena-a", "unstamped", nil, ours),
		policy("arena-a", "garbled", map[string]string{ExpiryAnnotation: "yesterday"}, ours),
		// Expired but not ours — a real partition an operator applied by
		// hand. Deleting it would be Simian breaking production.
		policy("arena-a", "not-ours", at(now.Add(-time.Hour)), nil),
		// Right label, right age, wrong namespace: not an arena we were
		// told about.
		policy("not-an-arena", "off-limits", at(now.Add(-time.Hour)), ours),
		policy("arena-b", "expired-elsewhere", at(now.Add(-time.Hour)), ours),
	)

	d := New(cs, "")
	cleared, err := d.ReapExpired(context.Background(), []string{"arena-a", "arena-b"}, now)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}

	sort.Strings(cleared)
	want := []string{"arena-a/expired", "arena-b/expired-elsewhere"}
	if !reflect.DeepEqual(cleared, want) {
		t.Errorf("cleared = %v, want %v", cleared, want)
	}
	survivors := map[string][]string{
		"arena-a":      {"garbled", "not-ours", "on-the-boundary", "still-running", "unstamped"},
		"arena-b":      nil,
		"not-an-arena": {"off-limits"},
	}
	for ns, want := range survivors {
		list, err := cs.NetworkingV1().NetworkPolicies(ns).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list %s: %v", ns, err)
		}
		var got []string
		for _, np := range list.Items {
			got = append(got, np.Name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("namespace %s left with %v, want %v", ns, got, want)
		}
	}
}

// A driver that gives up on the first bad namespace would leave real leaks in
// the healthy ones. RBAC is per-namespace, so one Forbidden is entirely
// plausible in a partly-configured install.
func TestReapExpiredKeepsGoingPastAFailedNamespace(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:        "expired",
		Namespace:   "arena-b",
		Labels:      map[string]string{ManagedLabel: "true"},
		Annotations: map[string]string{ExpiryAnnotation: now.Add(-time.Hour).Format(time.RFC3339)},
	}})
	cs.PrependReactor("list", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "arena-a" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New("nope"))
		}
		return false, nil, nil
	})

	d := New(cs, "")
	cleared, err := d.ReapExpired(context.Background(), []string{"arena-a", "arena-b"}, now)
	if err == nil {
		t.Fatal("expected the forbidden namespace to be reported")
	}
	if !strings.Contains(err.Error(), "arena-a") {
		t.Errorf("error should name the namespace that failed: %v", err)
	}
	if !reflect.DeepEqual(cleared, []string{"arena-b/expired"}) {
		t.Errorf("cleared = %v, want the healthy namespace still swept", cleared)
	}
}

// The reaper wiring in lease.Reaper finds the driver by type assertion. If the
// method signature drifts, that assertion silently stops matching and the
// orphan scan becomes a no-op with nothing failing to say so.
func TestDriverSatisfiesOrphanReaper(t *testing.T) {
	var _ simian.OrphanReaper = New(fake.NewSimpleClientset(), "")
}

func onlyPolicy(t *testing.T, cs *fake.Clientset, ns string) networkingv1.NetworkPolicy {
	t.Helper()
	list, err := cs.NetworkingV1().NetworkPolicies(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 NetworkPolicy in %s, got %d", ns, len(list.Items))
	}
	return list.Items[0]
}

func containsType(types []networkingv1.PolicyType, want networkingv1.PolicyType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}
