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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
