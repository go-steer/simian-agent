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

package arena

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func newManager() *Manager {
	return New(fake.NewClientset(), "simian-controller", "simian-system")
}

func TestCreateNamespaceAndRBAC(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	if err := m.Create(ctx, Spec{Namespace: "chaos-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ns, err := m.K8s.CoreV1().Namespaces().Get(ctx, "chaos-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if got := ns.Annotations[EligibilityAnnotation]; got != "true" {
		t.Errorf("annotation %s=%q, want true", EligibilityAnnotation, got)
	}
	if got := ns.Labels[ManagedByLabelKey]; got != ManagedByLabelValue {
		t.Errorf("label %s=%q, want %s", ManagedByLabelKey, got, ManagedByLabelValue)
	}

	role, err := m.K8s.RbacV1().Roles("chaos-1").Get(ctx, DefaultRoleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("role not created: %v", err)
	}
	if len(role.Rules) == 0 {
		t.Error("role has no rules")
	}

	rb, err := m.K8s.RbacV1().RoleBindings("chaos-1").Get(ctx, DefaultRoleBindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("rolebinding not created: %v", err)
	}
	if rb.RoleRef.Name != DefaultRoleName {
		t.Errorf("rolebinding role=%s, want %s", rb.RoleRef.Name, DefaultRoleName)
	}
	if len(rb.Subjects) != 1 ||
		rb.Subjects[0].Name != "simian-controller" ||
		rb.Subjects[0].Namespace != "simian-system" {
		t.Errorf("rolebinding subjects=%+v, want chaos-controller in simian-system", rb.Subjects)
	}
}

func TestCreateMergesExtraAnnotationsAndLabels(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	err := m.Create(ctx, Spec{
		Namespace: "chaos-2",
		ExtraAnnotations: map[string]string{
			ExcludeWorkloadsAnnotation: "loadgenerator,redis-cart",
			"team":                     "sre-eval",
		},
		ExtraLabels: map[string]string{"env": "test"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns, _ := m.K8s.CoreV1().Namespaces().Get(ctx, "chaos-2", metav1.GetOptions{})
	if got := ns.Annotations[EligibilityAnnotation]; got != "true" {
		t.Errorf("eligibility annotation lost: %q", got)
	}
	if got := ns.Annotations[ExcludeWorkloadsAnnotation]; got != "loadgenerator,redis-cart" {
		t.Errorf("exclude annotation: %q", got)
	}
	if got := ns.Annotations["team"]; got != "sre-eval" {
		t.Errorf("custom annotation: %q", got)
	}
	if got := ns.Labels["env"]; got != "test" {
		t.Errorf("custom label: %q", got)
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	if err := m.Create(ctx, Spec{Namespace: "chaos-3"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := m.Create(ctx, Spec{Namespace: "chaos-3"}); err != nil {
		t.Fatalf("second Create (idempotent): %v", err)
	}
}

func TestCreateRefusesPreexistingNonEligibleNamespace(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	_, _ = m.K8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "kube-system",
			Annotations: map[string]string{EligibilityAnnotation: "false"},
		},
	}, metav1.CreateOptions{})
	err := m.Create(ctx, Spec{Namespace: "kube-system"})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestDestroyRemovesAll(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	if err := m.Create(ctx, Spec{Namespace: "chaos-4"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Destroy(ctx, "chaos-4", false); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := m.K8s.CoreV1().Namespaces().Get(ctx, "chaos-4", metav1.GetOptions{}); err == nil {
		t.Error("namespace still exists after destroy")
	}
	if _, err := m.K8s.RbacV1().RoleBindings("chaos-4").Get(ctx, DefaultRoleBindingName, metav1.GetOptions{}); err == nil {
		t.Error("rolebinding still exists after destroy")
	}
}

func TestDestroyIdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	if err := m.Destroy(ctx, "never-existed", false); err != nil {
		t.Fatalf("Destroy on missing namespace should be idempotent, got: %v", err)
	}
}

func TestDescribeMissingNamespace(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	st, err := m.Describe(ctx, "nope")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if st.Exists {
		t.Error("expected Exists=false for missing namespace")
	}
}

func TestDescribePopulatesState(t *testing.T) {
	ctx := context.Background()
	m := newManager()
	if err := m.Create(ctx, Spec{
		Namespace: "chaos-5",
		ExtraAnnotations: map[string]string{
			ExcludeWorkloadsAnnotation: "loadgenerator,emailservice",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st, err := m.Describe(ctx, "chaos-5")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !st.Exists {
		t.Error("Exists should be true")
	}
	if !st.Eligible {
		t.Error("Eligible should be true")
	}
	if !st.RoleBindingExists {
		t.Error("RoleBindingExists should be true")
	}
	if !st.ChaosSubjectBound {
		t.Error("ChaosSubjectBound should be true")
	}
	if want := []string{"loadgenerator", "emailservice"}; !equalSlice(st.ExcludedWorkloads, want) {
		t.Errorf("ExcludedWorkloads=%v, want %v", st.ExcludedWorkloads, want)
	}
}

// newDynManager returns a Manager whose dynamic client knows the fault kinds
// activeFaultCount sweeps. The list kinds have to be named explicitly — the
// dynamic fake panics on a List it has no list kind for — and both maps are
// built from faultResources() so a new entry there is covered here for free.
//
// Objects are created through the client rather than seeded into the
// constructor because the fake guesses a resource name from the kind, and its
// guess for the chaos-mesh kinds is wrong ("networkchaoses"). Creating at the
// GVR activeFaultCount actually queries keeps the fixture honest.
func newDynManager(t *testing.T, objs ...*unstructured.Unstructured) *Manager {
	t.Helper()
	gvrToList := map[schema.GroupVersionResource]string{}
	gvrForKind := map[string]schema.GroupVersionResource{}
	for _, res := range faultResources() {
		gvrToList[res.gvr] = res.kind + "List"
		gvrForKind[res.kind] = res.gvr
	}

	m := New(fake.NewClientset(), "simian-controller", "simian-system")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToList)
	for _, obj := range objs {
		gvr, ok := gvrForKind[obj.GetKind()]
		if !ok {
			t.Fatalf("kind %q is not in faultResources()", obj.GetKind())
		}
		if _, err := dyn.Resource(gvr).Namespace(obj.GetNamespace()).
			Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		}
	}
	m.Dyn = dyn
	return m
}

func unstructuredFault(apiVersion, kind, ns, name string, managed bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	if managed {
		u.SetLabels(map[string]string{SimianManagedFaultLabel: "true"})
	}
	return u
}

// A NetworkPolicy partition is the one fault class that leaks forever: every
// Chaos Mesh kind carries a spec.duration the controller honours server-side,
// so those recover on their own. Leaving networkpolicies out of the sweep meant
// destroy tore down the arena while the partition it could not see was the only
// one that needed clearing.
func TestActiveFaultCountSeesNetworkPolicyPartitions(t *testing.T) {
	ctx := context.Background()
	var swept bool
	for _, res := range faultResources() {
		if res.gvr.Group == "networking.k8s.io" && res.gvr.Resource == "networkpolicies" {
			swept = true
		}
	}
	if !swept {
		t.Fatal("faultResources() does not sweep networkpolicies: a leaked partition is invisible to destroy and describe")
	}

	m := newDynManager(t,
		unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "chaos-np", "simian-np-01jt", true),
		unstructuredFault("chaos-mesh.org/v1alpha1", "NetworkChaos", "chaos-np", "simian-nc-1", true),
		// Not ours: an operator's own policy must not make destroy refuse.
		unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "chaos-np", "default-deny", false),
		// Ours, but in a different arena.
		unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "other-ns", "simian-np-zzz", true),
	)

	count, names, err := m.activeFaultCount(ctx, "chaos-np")
	if err != nil {
		t.Fatalf("activeFaultCount: %v", err)
	}
	want := []string{"NetworkChaos/simian-nc-1", "NetworkPolicy/simian-np-01jt"}
	if count != len(want) || !equalSlice(names, want) {
		t.Errorf("activeFaultCount = %d %v, want %d %v", count, names, len(want), want)
	}
}

// A dynamic List can return items with an empty kind — the kind is implied by
// the list's own kind rather than repeated per item. Without the per-resource
// fallback the destroy refusal reads "/simian-np-01jt", naming the leak
// without saying what it is or which engine to clear it with.
func TestActiveFaultNamesSurviveAnItemWithNoKind(t *testing.T) {
	m := newDynManager(t)
	m.Dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "networkpolicies",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			item := unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "chaos-np", "simian-np-01jt", true)
			item.SetKind("")
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*item}}, nil
		})

	_, names, err := m.activeFaultCount(context.Background(), "chaos-np")
	if err != nil {
		t.Fatalf("activeFaultCount: %v", err)
	}
	if !equalSlice(names, []string{"NetworkPolicy/simian-np-01jt"}) {
		t.Errorf("names = %v, want [NetworkPolicy/simian-np-01jt]", names)
	}
}

// The count alone tells an operator that destroy refused, not what to clear.
func TestDestroyRefusalNamesTheLeakedPolicy(t *testing.T) {
	ctx := context.Background()
	m := newDynManager(t, unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "chaos-np", "simian-np-01jt", true))
	if err := m.Create(ctx, Spec{Namespace: "chaos-np"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := m.Destroy(ctx, "chaos-np", false)
	if err == nil {
		t.Fatal("destroy should refuse while a simian-managed NetworkPolicy is live")
	}
	if !strings.Contains(err.Error(), "NetworkPolicy/simian-np-01jt") {
		t.Errorf("refusal should name the resource to clear; got: %v", err)
	}

	if err := m.Destroy(ctx, "chaos-np", true); err != nil {
		t.Fatalf("--force should still destroy: %v", err)
	}
}

func TestDescribeListsActiveFaultNames(t *testing.T) {
	ctx := context.Background()
	m := newDynManager(t, unstructuredFault("networking.k8s.io/v1", "NetworkPolicy", "chaos-np", "simian-np-01jt", true))
	if err := m.Create(ctx, Spec{Namespace: "chaos-np"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st, err := m.Describe(ctx, "chaos-np")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if st.SimianFaultCount != 1 || !equalSlice(st.SimianFaultNames, []string{"NetworkPolicy/simian-np-01jt"}) {
		t.Errorf("SimianFaultCount=%d names=%v, want 1 [NetworkPolicy/simian-np-01jt]",
			st.SimianFaultCount, st.SimianFaultNames)
	}
}

// The arena Role is created three ways — roleRules() at runtime, the raw
// manifest, and the Helm chart — and only the first is exercised by a test.
// #50 was exactly this: a verb added in one place and missing in the others.
func TestRoleRulesMatchTheShippedManifests(t *testing.T) {
	repoRoot := func() string {
		_, thisFile, _, ok := goruntime.Caller(0)
		if !ok {
			t.Fatal("cannot locate this test file")
		}
		return filepath.Join(filepath.Dir(thisFile), "..", "..")
	}()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"raw manifest", filepath.Join(repoRoot, "deploy", "manifests", "00-rbac.yaml")},
		{"helm chart", filepath.Join(repoRoot, "deploy", "helm", "simian", "templates", "serviceaccount.yaml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			rules := chaosRoleRulesFromYAML(t, string(raw))
			if diff := diffRules(roleRules(), rules); diff != "" {
				t.Errorf("%s is out of sync with roleRules() in pkg/arena/arena.go:\n%s", tc.path, diff)
			}
		})
	}
}

// chaosRoleRulesFromYAML pulls the rules of the simian-chaos Role out of a
// multi-document manifest.
//
// The Helm template is not valid YAML — it carries {{ }} actions and, in the
// sutInController block, a conditional that adds rules. Rather than render it
// (which would make the test depend on a helm binary being installed), strip
// template lines and the conditional block: what remains is the unconditional
// ruleset, which is what must match roleRules().
func chaosRoleRulesFromYAML(t *testing.T, raw string) []rbacv1.PolicyRule {
	t.Helper()

	var kept []string
	skipping := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "{{- if"), strings.HasPrefix(trimmed, "{{ if"):
			skipping = true
		case strings.HasPrefix(trimmed, "{{- end"), strings.HasPrefix(trimmed, "{{ end"):
			skipping = false
		case skipping, strings.HasPrefix(trimmed, "{{"):
			// template action or inside a conditional: drop
		default:
			// Placeholders like ${EligibleNamespace} and inline {{ . }} in
			// metadata are fine for YAML parsing as bare scalars.
			kept = append(kept, strings.ReplaceAll(line, "{{ . }}", "placeholder-ns"))
		}
	}

	for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
		var role rbacv1.Role
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			continue // not a Role, or a document the strip left ragged
		}
		if role.Kind == "Role" && role.Name == DefaultRoleName {
			return role.Rules
		}
	}
	t.Fatalf("no %q Role found in manifest", DefaultRoleName)
	return nil
}

// diffRules compares rulesets as sets of (apiGroup, resource, verb) triples.
// Rule ordering and how rules are grouped are cosmetic; the granted permissions
// are not.
func diffRules(want, got []rbacv1.PolicyRule) string {
	flatten := func(rules []rbacv1.PolicyRule) map[string]bool {
		out := map[string]bool{}
		for _, r := range rules {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						out[fmt.Sprintf("%s/%s:%s", g, res, v)] = true
					}
				}
			}
		}
		return out
	}
	wantSet, gotSet := flatten(want), flatten(got)
	var missing, extra []string
	for k := range wantSet {
		if !gotSet[k] {
			missing = append(missing, k)
		}
	}
	for k := range gotSet {
		if !wantSet[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	var b strings.Builder
	if len(missing) > 0 {
		fmt.Fprintf(&b, "  granted by roleRules() but missing from the manifest: %s\n", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "  in the manifest but not granted by roleRules():      %s\n", strings.Join(extra, ", "))
	}
	return b.String()
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
