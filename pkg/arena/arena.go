// Package arena owns the eligibility-and-RBAC half of the M2 provisioner work.
// An "arena" is a Kubernetes namespace that has been marked eligible for
// Simian-driven chaos and bound to the chaos ServiceAccount via a Role +
// RoleBinding. Arenas are the chaos jail: any Simian fault application is
// rejected by the executor unless its target namespace is an arena.
//
// This package is engine-agnostic — it does not know about Chaos Mesh, Litmus,
// or the Fault Executor. It only knows the eligibility model defined in
// docs/design.md §10.3.
package arena

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Annotation keys used on arena namespaces. Kept here (not in pkg/executor) so
// arena setup and executor enforcement share a single source of truth.
const (
	// EligibilityAnnotation marks a namespace as a chaos arena. Value must be "true".
	EligibilityAnnotation = "simian.chaos/eligible"

	// ExcludeWorkloadsAnnotation is an optional comma-separated list of workload
	// names the executor must never target inside this arena.
	ExcludeWorkloadsAnnotation = "simian.chaos/exclude-workloads"

	// ManagedByLabelValue / ManagedByLabelKey label resources Simian created.
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "simian-arena"

	// SimianManagedFaultLabel labels chaos resources Simian's executor created.
	// Used here only for the destroy-time active-fault check.
	SimianManagedFaultLabel = "simian.chaos/managed"
)

// Default RBAC names. Production deployments may override via Manager fields.
const (
	DefaultRoleName        = "simian-chaos"
	DefaultRoleBindingName = "simian-chaos"
)

// Manager performs arena CRUD against a Kubernetes cluster.
//
// All cluster mutations the manager performs require provisioner-level
// privileges (namespace create/delete, RoleBinding create/delete). The CLI
// path that wraps this manager runs with the operator's kubeconfig; in-cluster
// invocation would mount the provisioner ServiceAccount.
type Manager struct {
	K8s kubernetes.Interface

	// Dyn is optional. When set, Destroy uses it to look for
	// simian-managed chaos resources in the namespace and refuse the destroy
	// unless force=true. When nil, Destroy skips that check.
	Dyn dynamic.Interface

	// ChaosSAName / ChaosSANamespace identify the controller ServiceAccount
	// the arena's RoleBinding grants chaos-engine access to.
	ChaosSAName      string
	ChaosSANamespace string

	// RoleName / RoleBindingName allow tests and non-default installations to
	// override the resource names. Empty values fall back to defaults above.
	RoleName        string
	RoleBindingName string
}

// New constructs a Manager with default role names and the given chaos SA.
func New(k8s kubernetes.Interface, chaosSAName, chaosSANS string) *Manager {
	return &Manager{
		K8s:              k8s,
		ChaosSAName:      chaosSAName,
		ChaosSANamespace: chaosSANS,
		RoleName:         DefaultRoleName,
		RoleBindingName:  DefaultRoleBindingName,
	}
}

// Spec describes the arena to create.
type Spec struct {
	// Namespace is the target namespace name. Required.
	Namespace string
	// ExtraAnnotations are merged on top of the eligibility annotation.
	// (e.g. simian.chaos/exclude-workloads=loadgenerator,redis-cart)
	ExtraAnnotations map[string]string
	// ExtraLabels are merged on top of the managed-by label.
	ExtraLabels map[string]string
}

// State is the current view of an arena, suitable for Describe.
type State struct {
	Namespace          string
	Exists             bool
	Eligible           bool
	Annotations        map[string]string
	ExcludedWorkloads  []string
	RoleBindingExists  bool
	ChaosSubjectBound  bool
	SimianFaultCount   int // count of simian-managed chaos resources discovered
}

// roleRules is the canonical RBAC ruleset granted to the chaos SA inside an
// arena. Mirrors deploy/manifests/00-rbac.yaml's per-eligible-NS Role.
func roleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"chaos-mesh.org"},
			Resources: []string{"*"},
			Verbs:     []string{"create", "get", "list", "watch", "patch", "delete"},
		},
		{
			APIGroups: []string{"litmuschaos.io"},
			Resources: []string{"chaosengines", "chaosresults", "chaosschedules"},
			Verbs:     []string{"create", "get", "list", "watch", "patch", "delete"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods", "pods/log", "events", "configmaps", "services"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
}

// Create provisions an arena: namespace + Role + RoleBinding. Idempotent —
// re-creating an arena that already exists with matching configuration is a
// no-op; mismatched configuration returns an error so the operator can decide
// whether to repair manually.
func (m *Manager) Create(ctx context.Context, spec Spec) error {
	if spec.Namespace == "" {
		return fmt.Errorf("arena: namespace is required")
	}
	if m.ChaosSAName == "" || m.ChaosSANamespace == "" {
		return fmt.Errorf("arena: chaos ServiceAccount name and namespace are required")
	}

	annotations := map[string]string{EligibilityAnnotation: "true"}
	for k, v := range spec.ExtraAnnotations {
		annotations[k] = v
	}
	labels := map[string]string{ManagedByLabelKey: ManagedByLabelValue}
	for k, v := range spec.ExtraLabels {
		labels[k] = v
	}

	if err := m.upsertNamespace(ctx, spec.Namespace, annotations, labels); err != nil {
		return err
	}
	if err := m.upsertRole(ctx, spec.Namespace, labels); err != nil {
		return err
	}
	if err := m.upsertRoleBinding(ctx, spec.Namespace, labels); err != nil {
		return err
	}
	return nil
}

func (m *Manager) upsertNamespace(ctx context.Context, name string, annotations, lbls map[string]string) error {
	ns, err := m.K8s.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = m.K8s.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      lbls,
				Annotations: annotations,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("arena: create namespace %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("arena: get namespace %q: %w", name, err)
	}
	// Namespace exists. Verify eligibility annotation matches; refuse to
	// overwrite an existing-but-not-managed-by-Simian namespace silently.
	current, ok := ns.Annotations[EligibilityAnnotation]
	if ok && current != "true" {
		return fmt.Errorf("arena: namespace %q already has %s=%q; refusing to overwrite",
			name, EligibilityAnnotation, current)
	}
	if !ok || hasMissingAnnotations(ns.Annotations, annotations) {
		// Patch in our annotations + labels.
		merged := mergeMap(ns.Annotations, annotations)
		mergedLbls := mergeMap(ns.Labels, lbls)
		ns.Annotations = merged
		ns.Labels = mergedLbls
		_, err = m.K8s.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("arena: update namespace %q annotations: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) upsertRole(ctx context.Context, namespace string, lbls map[string]string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.roleName(),
			Namespace: namespace,
			Labels:    lbls,
		},
		Rules: roleRules(),
	}
	_, err := m.K8s.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Update to keep the rules in sync with the canonical roleRules() set.
		existing, getErr := m.K8s.RbacV1().Roles(namespace).Get(ctx, m.roleName(), metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("arena: get existing role: %w", getErr)
		}
		existing.Rules = roleRules()
		existing.Labels = mergeMap(existing.Labels, lbls)
		_, err = m.K8s.RbacV1().Roles(namespace).Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("arena: update role %s/%s: %w", namespace, m.roleName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("arena: create role %s/%s: %w", namespace, m.roleName(), err)
	}
	return nil
}

func (m *Manager) upsertRoleBinding(ctx context.Context, namespace string, lbls map[string]string) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.roleBindingName(),
			Namespace: namespace,
			Labels:    lbls,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     m.roleName(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      m.ChaosSAName,
				Namespace: m.ChaosSANamespace,
			},
		},
	}
	_, err := m.K8s.RbacV1().RoleBindings(namespace).Create(ctx, rb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := m.K8s.RbacV1().RoleBindings(namespace).Get(ctx, m.roleBindingName(), metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("arena: get existing rolebinding: %w", getErr)
		}
		existing.RoleRef = rb.RoleRef
		existing.Subjects = rb.Subjects
		existing.Labels = mergeMap(existing.Labels, lbls)
		_, err = m.K8s.RbacV1().RoleBindings(namespace).Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("arena: update rolebinding %s/%s: %w", namespace, m.roleBindingName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("arena: create rolebinding %s/%s: %w", namespace, m.roleBindingName(), err)
	}
	return nil
}

// Destroy removes an arena's RoleBinding, Role, and namespace. If the
// namespace contains active simian-managed chaos resources and force=false,
// returns an error that names them so the caller can clear them first via
// the executor's Clear method.
func (m *Manager) Destroy(ctx context.Context, namespace string, force bool) error {
	if namespace == "" {
		return fmt.Errorf("arena: namespace is required")
	}
	if !force && m.Dyn != nil {
		count, names, err := m.activeFaultCount(ctx, namespace)
		if err != nil {
			return fmt.Errorf("arena: active-fault check: %w", err)
		}
		if count > 0 {
			return fmt.Errorf("arena: %d simian-managed chaos resource(s) still active in %q (%s); clear them first or pass --force",
				count, namespace, strings.Join(names, ", "))
		}
	}

	if err := m.K8s.RbacV1().RoleBindings(namespace).Delete(ctx, m.roleBindingName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("arena: delete rolebinding: %w", err)
	}
	if err := m.K8s.RbacV1().Roles(namespace).Delete(ctx, m.roleName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("arena: delete role: %w", err)
	}
	if err := m.K8s.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("arena: delete namespace: %w", err)
	}
	return nil
}

// Describe returns a snapshot of an arena's current state. Returns
// State{Exists:false} for missing namespaces (without error).
func (m *Manager) Describe(ctx context.Context, namespace string) (State, error) {
	out := State{Namespace: namespace}
	ns, err := m.K8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("arena: get namespace: %w", err)
	}
	out.Exists = true
	out.Annotations = ns.Annotations
	out.Eligible = ns.Annotations[EligibilityAnnotation] == "true"
	if v, ok := ns.Annotations[ExcludeWorkloadsAnnotation]; ok && v != "" {
		for _, w := range strings.Split(v, ",") {
			if w = strings.TrimSpace(w); w != "" {
				out.ExcludedWorkloads = append(out.ExcludedWorkloads, w)
			}
		}
	}
	rb, err := m.K8s.RbacV1().RoleBindings(namespace).Get(ctx, m.roleBindingName(), metav1.GetOptions{})
	if err == nil {
		out.RoleBindingExists = true
		for _, s := range rb.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == m.ChaosSAName && s.Namespace == m.ChaosSANamespace {
				out.ChaosSubjectBound = true
				break
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return out, fmt.Errorf("arena: get rolebinding: %w", err)
	}

	if m.Dyn != nil {
		count, _, err := m.activeFaultCount(ctx, namespace)
		if err == nil {
			out.SimianFaultCount = count
		}
	}
	return out, nil
}

// activeFaultCount lists simian-labeled chaos resources in the namespace
// across both engines and returns count + sorted names. Best-effort: missing
// CRDs are not errors.
func (m *Manager) activeFaultCount(ctx context.Context, namespace string) (int, []string, error) {
	if m.Dyn == nil {
		return 0, nil, nil
	}
	selector := labels.SelectorFromSet(labels.Set{SimianManagedFaultLabel: "true"}).String()
	gvrs := []schema.GroupVersionResource{
		// Chaos Mesh user-facing fault types we know about. Listing a missing
		// CRD just returns NotFound; we ignore it.
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "networkchaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "podchaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "iochaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "stresschaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "timechaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "kernelchaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "dnschaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "httpchaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "jvmchaos"},
		{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: "blockchaos"},
		{Group: "litmuschaos.io", Version: "v1alpha1", Resource: "chaosengines"},
	}
	var names []string
	for _, gvr := range gvrs {
		list, err := m.Dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			if apierrors.IsNotFound(err) || isNoMatchError(err) {
				continue
			}
			return 0, nil, err
		}
		for _, item := range list.Items {
			names = append(names, fmt.Sprintf("%s/%s", item.GetKind(), item.GetName()))
		}
	}
	sort.Strings(names)
	return len(names), names, nil
}

func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	// dynamic client returns a "no matches for kind" error wrapper when CRD
	// isn't installed; check via string match (avoids importing meta package
	// for one error type).
	return strings.Contains(err.Error(), "no matches for kind") ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}

func (m *Manager) roleName() string {
	if m.RoleName == "" {
		return DefaultRoleName
	}
	return m.RoleName
}

func (m *Manager) roleBindingName() string {
	if m.RoleBindingName == "" {
		return DefaultRoleBindingName
	}
	return m.RoleBindingName
}

func mergeMap(base, overlay map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

func hasMissingAnnotations(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return true
		}
	}
	return false
}
