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

// Package networkpolicy implements simian.ChaosDriver using standard
// networking.k8s.io/v1 NetworkPolicy resources to simulate network
// partitions. It exists because Chaos Mesh's NetworkChaos is silently
// bypassed on GKE Dataplane V2 (eBPF/Cilium); see
// https://go-steer.github.io/simian-agent/docs/dpv2-chaos-engines/
// for the full rationale.
//
// The engine only supports partition-style chaos — it can deny ingress,
// egress, or both for a labeled set of pods. It does NOT implement delay,
// loss, jitter, or bandwidth shaping. For HTTP-layer latency or aborts on
// DPv2, use the envoy-fault engine instead.
package networkpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Kind is the single resource kind this engine emits in its catalog.
const Kind = "NetworkPolicy"

// APIVersion is the standard K8s networking API version.
const APIVersion = "networking.k8s.io/v1"

// ManagedLabel marks a NetworkPolicy as Simian's. Mirrors
// arena.SimianManagedFaultLabel, duplicated rather than imported to keep the
// driver from depending on the arena package.
const ManagedLabel = "simian.chaos/managed"

// FaultUIDLabel ties the policy back to the fault that created it.
const FaultUIDLabel = "simian.chaos/fault-uid"

// ExpiryAnnotation carries the fault's deadline as an RFC3339 timestamp.
//
// This is the durable half of the lease. Every Chaos Mesh resource carries a
// spec.duration that the chaos-controller-manager honours server-side, so those
// faults recover on their own even if Simian is killed. A NetworkPolicy has no
// such field: without something written into the cluster, a partition applied
// by a process that then dies is permanent, and the in-memory lease registry
// that was supposed to clear it died with the process.
//
// Recording the deadline on the object itself means the cluster holds enough
// state for any later Simian — a restart, or a different replica — to find the
// orphan and reap it. See ReapExpired.
const ExpiryAnnotation = "simian.chaos/expires-at"

// Driver implements simian.ChaosDriver for NetworkPolicy-based partitions.
type Driver struct {
	clientset  kubernetes.Interface
	namePrefix string

	// Now is the clock used to stamp expiries. Nil means time.Now; tests
	// override it to make the stamp assertable.
	Now func() time.Time
}

// New creates a Driver. namePrefix is the GenerateName prefix; defaults to
// "simian-np-".
func New(clientset kubernetes.Interface, namePrefix string) *Driver {
	if namePrefix == "" {
		namePrefix = "simian-np-"
	}
	return &Driver{clientset: clientset, namePrefix: namePrefix}
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Engine implements ChaosDriver.
func (d *Driver) Engine() simian.Engine { return simian.EngineNetworkPolicy }

// Apply implements ChaosDriver. The manifest spec uses a simplified shape
// the LLM finds easier than the verbose K8s NetworkPolicy schema:
//
//	{"labelSelectors": {"app": "frontend"}, "directions": ["ingress","egress"]}
//
// directions is optional; default is both. The driver translates this into
// a real NetworkPolicy with empty ingress/egress arrays (= deny all matching
// the listed policyTypes).
func (d *Driver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if len(m.Targets) == 0 {
		return "", fmt.Errorf("network-policy apply: manifest has no targets")
	}
	ns := m.Targets[0].Namespace
	if ns == "" {
		return "", fmt.Errorf("network-policy apply: manifest target has no namespace")
	}

	labels, err := extractLabelSelectors(m.Spec)
	if err != nil {
		return "", fmt.Errorf("network-policy apply: %w", err)
	}
	directions, err := extractDirections(m.Spec)
	if err != nil {
		return "", fmt.Errorf("network-policy apply: %w", err)
	}

	policyTypes := make([]networkingv1.PolicyType, 0, 2)
	for _, dir := range directions {
		switch strings.ToLower(dir) {
		case "ingress":
			policyTypes = append(policyTypes, networkingv1.PolicyTypeIngress)
		case "egress":
			policyTypes = append(policyTypes, networkingv1.PolicyTypeEgress)
		default:
			return "", fmt.Errorf("network-policy apply: invalid direction %q (must be ingress or egress)", dir)
		}
	}

	// Generate a deterministic suffix ourselves rather than relying on
	// API-server GenerateName: the suffix becomes part of the engineUID we
	// return, and we want the same uid to be observable from tests against
	// the fake clientset (which doesn't honor GenerateName).
	name := d.namePrefix + strings.ToLower(ulid.Make().String())
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				ManagedLabel:  "true",
				FaultUIDLabel: m.UID,
			},
			Annotations: expiryAnnotation(d.now(), m.Duration),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: policyTypes,
			// Empty Ingress/Egress slices alongside the policyType = deny-all
			// for that direction. We deliberately do NOT set the field to nil
			// (which would mean "no rule"); we set it to an empty rule list.
			Ingress: emptyIngressIfTyped(policyTypes),
			Egress:  emptyEgressIfTyped(policyTypes),
		},
	}

	created, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("network-policy apply: create: %w", err)
	}
	return engineUID(created.GetNamespace(), created.GetName()), nil
}

// Clear implements ChaosDriver. Idempotent — NotFound is treated as success.
func (d *Driver) Clear(ctx context.Context, engineUIDStr string) error {
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		return err
	}
	err = d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("network-policy clear %s/%s: %w", ns, name, err)
	}
	return nil
}

// ReapExpired deletes every Simian-managed NetworkPolicy in the given
// namespaces whose expiry has passed, and returns the engineUIDs it cleared.
//
// This is the recovery path for a partition whose lease died with the process
// that held it. lease.Reaper clears faults it knows about; a policy created by
// a previous Simian is not in the registry of the current one, so nothing in
// memory will ever release it. Reading the deadline back off the object closes
// that gap: whichever Simian is running next sweeps up what the last one left.
//
// Policies with no expiry annotation are left alone. That is deliberate — an
// unstamped policy was either written by a Simian older than this annotation
// or by something else entirely wearing our label, and deleting a partition we
// cannot prove is expired is worse than leaving it for an operator to see. It
// still shows up in `simian arena describe`.
//
// Errors on individual namespaces are collected rather than returned early: a
// missing or forbidden namespace must not stop the other namespaces from being
// swept. Whatever was cleared is returned alongside the error.
func (d *Driver) ReapExpired(ctx context.Context, namespaces []string, now time.Time) ([]string, error) {
	var (
		cleared []string
		errs    []error
	)
	for _, ns := range namespaces {
		list, err := d.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{
			LabelSelector: ManagedLabel + "=true",
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("network-policy reap: list in %s: %w", ns, err))
			continue
		}
		for i := range list.Items {
			np := &list.Items[i]
			expiry, ok := parseExpiry(np.Annotations)
			if !ok || !expiry.Before(now) {
				continue
			}
			err := d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(ctx, np.Name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("network-policy reap: delete %s/%s: %w", ns, np.Name, err))
				continue
			}
			cleared = append(cleared, engineUID(ns, np.Name))
		}
	}
	return cleared, errors.Join(errs...)
}

// expiryAnnotation builds the annotation map for a fault of the given
// duration. A non-positive duration means the caller did not set one; rather
// than stamping a deadline that has already passed (which would make the next
// sweep delete a live fault), we stamp nothing and leave the policy to the
// in-memory lease.
func expiryAnnotation(now time.Time, d time.Duration) map[string]string {
	if d <= 0 {
		return nil
	}
	return map[string]string{ExpiryAnnotation: now.Add(d).UTC().Format(time.RFC3339)}
}

// parseExpiry reads the deadline back. An unparseable value is treated as
// absent — same reasoning as a missing one: never delete on a guess.
func parseExpiry(ann map[string]string) (time.Time, bool) {
	raw, ok := ann[ExpiryAnnotation]
	if !ok || raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Catalog implements ChaosDriver. Always returns a single entry; the
// NetworkPolicy API is unconditionally available in any modern Kubernetes
// cluster (it has been GA since 1.7), so there is no installation check.
func (d *Driver) Catalog(_ context.Context) ([]simian.CatalogEntry, error) {
	return []simian.CatalogEntry{
		{
			Engine:          simian.EngineNetworkPolicy,
			APIVersion:      APIVersion,
			ResourceKind:    Kind,
			BlastRadiusTier: catalog.Classify(simian.EngineNetworkPolicy, Kind),
			Description:     "Standard K8s NetworkPolicy partition (works on GKE Dataplane V2; partition only, no delay/loss).",
			EfficacyGate:    catalog.EfficacyGate(simian.EngineNetworkPolicy, Kind),
			SpecTemplate: `Use this for partition-style chaos: completely cut off ingress, egress,
or both for a labeled set of pods. Works on GKE Dataplane V2.

NOT for delay/loss/jitter — only on/off connectivity. For HTTP-layer
delay or abort, use engine=envoy-fault instead.

Spec:
  {"labelSelectors": {"app": "<workload>"},
   "directions": ["ingress", "egress"]}

  - labelSelectors: required; pod label match (e.g. {"app": "frontend"}).
  - directions: optional list; values "ingress" and/or "egress".
                Defaults to both if omitted.`,
		},
	}, nil
}

// extractLabelSelectors pulls labelSelectors out of the manifest spec.
// Required field; returns an error if missing or empty.
func extractLabelSelectors(spec map[string]any) (map[string]string, error) {
	raw, ok := spec["labelSelectors"]
	if !ok {
		return nil, fmt.Errorf("spec.labelSelectors is required")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec.labelSelectors must be an object")
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("spec.labelSelectors must not be empty")
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("spec.labelSelectors[%q] must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

// extractDirections pulls the optional directions list out of the manifest
// spec. Defaults to ["ingress","egress"] if absent.
func extractDirections(spec map[string]any) ([]string, error) {
	raw, ok := spec["directions"]
	if !ok {
		return []string{"ingress", "egress"}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.directions must be an array of strings")
	}
	if len(arr) == 0 {
		return []string{"ingress", "egress"}, nil
	}
	out := make([]string, 0, len(arr))
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("spec.directions[%d] must be a string", i)
		}
		out = append(out, s)
	}
	return out, nil
}

// emptyIngressIfTyped returns an empty []NetworkPolicyIngressRule (= "deny
// all ingress") when policyTypes contains Ingress, otherwise nil. The
// distinction matters: nil means "no rule, but still subject to default
// allow"; an empty slice paired with PolicyTypeIngress means "deny all".
func emptyIngressIfTyped(types []networkingv1.PolicyType) []networkingv1.NetworkPolicyIngressRule {
	for _, t := range types {
		if t == networkingv1.PolicyTypeIngress {
			return []networkingv1.NetworkPolicyIngressRule{}
		}
	}
	return nil
}

func emptyEgressIfTyped(types []networkingv1.PolicyType) []networkingv1.NetworkPolicyEgressRule {
	for _, t := range types {
		if t == networkingv1.PolicyTypeEgress {
			return []networkingv1.NetworkPolicyEgressRule{}
		}
	}
	return nil
}

func engineUID(namespace, name string) string {
	return namespace + "/" + name
}

func decodeEngineUID(s string) (string, string, error) {
	idx := strings.Index(s, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid engineUID %q", s)
	}
	return s[:idx], s[idx+1:], nil
}
