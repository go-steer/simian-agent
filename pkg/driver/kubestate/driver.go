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

// Package kubestate implements simian.ChaosDriver for declarative-state
// faults: an object that is wrong in the API server, rather than a
// perturbation of a running dataplane.
//
// Simian's other four engines all break traffic or processes. That is half the
// failure space, and not the half an SRE agent spends most of its time in — a
// wedged rollout, an image that does not exist, a pod nothing can schedule and
// a container that dies on startup are all states, not events, and none of
// them can be produced by delaying a packet or killing a process. This engine
// produces the other half.
//
// It operates in two modes. `synthesize`, implemented here, applies a bundle
// that is born broken into an arena Simian created: deterministic, independent
// of what is already running, and safe to compare against a baseline captured
// before it existed. `mutate` — patching an existing healthy workload and
// reverting on clear — is the strategic mode and is not implemented yet.
//
// No new privileged path: the driver sits behind the same ChaosDriver
// interface, the same executor chokepoint, the same lease and the same reaper
// as every other engine. It does need one RBAC grant the others do not —
// create/delete on apps/deployments inside arena namespaces — which is added
// to the arena Role in pkg/arena.
package kubestate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// APIVersion is the apiVersion a manifest must carry to reach this engine.
// The synthesized object is an apps/v1 Deployment, and saying so keeps the
// manifest honest about what will appear in the cluster.
const APIVersion = "apps/v1"

// ManagedLabel marks a Deployment as Simian's. Mirrors
// arena.SimianManagedFaultLabel, duplicated rather than imported to keep the
// driver from depending on the arena package.
const ManagedLabel = "simian.chaos/managed"

// FaultUIDLabel ties the workload back to the fault that created it.
const FaultUIDLabel = "simian.chaos/fault-uid"

// KindLabel records which fault kind synthesized it, so `kubectl get deploy -L`
// in a wrecked arena says what happened without a round trip to the audit log.
const KindLabel = "simian.chaos/kind"

// ExpiryAnnotation carries the fault's deadline as an RFC3339 timestamp.
//
// Same reasoning as the network-policy driver: a Chaos Mesh resource has a
// server-side spec.duration and recovers on its own if Simian dies, and a
// plain Kubernetes object does not. A synthesized Deployment left behind by a
// process that was killed mid-fault is a crash-looping workload in the
// operator's namespace forever. Writing the deadline onto the object means
// whichever Simian runs next has enough state to clean it up. See ReapExpired.
const ExpiryAnnotation = "simian.chaos/expires-at"

// Driver implements simian.ChaosDriver for synthesized declarative-state
// faults.
type Driver struct {
	clientset kubernetes.Interface

	// Now is the clock used to stamp expiries. Nil means time.Now; tests
	// override it to make the stamp assertable.
	Now func() time.Time
}

// New creates a Driver.
func New(clientset kubernetes.Interface) *Driver {
	return &Driver{clientset: clientset}
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Engine implements ChaosDriver.
func (d *Driver) Engine() simian.Engine { return simian.EngineKubeState }

// Apply implements ChaosDriver. It creates one Deployment that is born in the
// failure state named by m.ResourceKind.
//
// The spec is small on purpose — every field has a default that produces the
// target state, so `{}` is a valid spec for all four kinds and the planner has
// nothing it must get right in order to inject a working fault.
func (d *Driver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if len(m.Targets) == 0 {
		return "", fmt.Errorf("kube-state apply: manifest has no targets")
	}
	ns := m.Targets[0].Namespace
	if ns == "" {
		return "", fmt.Errorf("kube-state apply: manifest target has no namespace")
	}
	if err := checkMode(m.Spec); err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}
	build, ok := builders[m.ResourceKind]
	if !ok {
		return "", fmt.Errorf("kube-state apply: unsupported kind %q (want one of %s)",
			m.ResourceKind, strings.Join(Kinds(), ", "))
	}
	name, err := workloadName(m.ResourceKind, m.Spec, m.UID)
	if err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}
	replicas, err := optInt(m.Spec, "replicas", 1)
	if err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}
	if replicas < 1 || replicas > maxReplicas {
		return "", fmt.Errorf("kube-state apply: spec.replicas must be between 1 and %d, got %d", maxReplicas, replicas)
	}
	pod, err := build(m.Spec)
	if err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}

	dep := newDeployment(name, ns, int32(replicas), m.ResourceKind, m.UID,
		expiryAnnotation(d.now(), m.Duration), pod)
	created, err := d.clientset.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("kube-state apply: create deployment %s/%s: %w", ns, name, err)
	}
	return engineUID(created.GetNamespace(), created.GetName()), nil
}

// Clear implements ChaosDriver. Idempotent — NotFound is treated as success.
//
// Deleting the Deployment garbage-collects its ReplicaSet and pods, so a
// synthesized fault leaves nothing behind. That is the whole recovery story
// for synthesize mode: nothing that existed before the fault was touched, so
// there is nothing to restore.
func (d *Driver) Clear(ctx context.Context, engineUIDStr string) error {
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		return err
	}
	err = d.clientset.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube-state clear %s/%s: %w", ns, name, err)
	}
	return nil
}

// ReapExpired deletes every Simian-managed Deployment in the given namespaces
// whose expiry has passed, and returns the engineUIDs it cleared.
//
// Deployments with no expiry annotation are left alone, for the same reason
// the network-policy driver leaves unstamped policies: an unstamped object was
// written either by an older Simian or by something else wearing our label,
// and deleting a workload we cannot prove is expired is worse than leaving it
// somewhere an operator will see it.
func (d *Driver) ReapExpired(ctx context.Context, namespaces []string, now time.Time) ([]string, error) {
	var (
		cleared []string
		errs    []error
	)
	for _, ns := range namespaces {
		list, err := d.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
			LabelSelector: ManagedLabel + "=true",
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("kube-state reap: list in %s: %w", ns, err))
			continue
		}
		for i := range list.Items {
			dep := &list.Items[i]
			expiry, ok := parseExpiry(dep.Annotations)
			if !ok || !expiry.Before(now) {
				continue
			}
			err := d.clientset.AppsV1().Deployments(ns).Delete(ctx, dep.Name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("kube-state reap: delete %s/%s: %w", ns, dep.Name, err))
				continue
			}
			cleared = append(cleared, engineUID(ns, dep.Name))
		}
	}
	return cleared, errors.Join(errs...)
}

// Catalog implements ChaosDriver. The entries are static: this engine creates
// core apps/v1 Deployments, which exist in every cluster, so unlike the Chaos
// Mesh catalog there is nothing to discover and nothing that can be missing.
func (d *Driver) Catalog(_ context.Context) ([]simian.CatalogEntry, error) {
	out := make([]simian.CatalogEntry, 0, len(builders))
	for _, kind := range Kinds() {
		out = append(out, simian.CatalogEntry{
			Engine:          simian.EngineKubeState,
			APIVersion:      APIVersion,
			ResourceKind:    kind,
			BlastRadiusTier: catalog.Classify(simian.EngineKubeState, kind),
			Description:     descriptions[kind],
			EfficacyGate:    catalog.EfficacyGate(simian.EngineKubeState, kind),
			SpecTemplate:    specTemplates[kind],
		})
	}
	return out, nil
}

var descriptions = map[string]string{
	KindImageUnresolvable:  "Synthesize a workload whose image reference resolves to no manifest, producing ErrImagePull / ImagePullBackOff.",
	KindContainerExitLoop:  "Synthesize a workload whose process exits non-zero on startup, producing CrashLoopBackOff.",
	KindMemoryLimitSqueeze: "Synthesize a workload whose working set exceeds its own memory limit, producing OOMKilled.",
	KindUnschedulable:      "Synthesize a workload the scheduler cannot place, producing a Pending pod and FailedScheduling events.",
}

// commonSpecNotes is appended to every kind's template. Repeating it per entry
// rather than stating it once is deliberate: the planner sees one entry at a
// time.
const commonSpecNotes = `
Common to every kube-state kind:
  - mode:     optional, "synthesize" (default). Creates a new broken workload
              in the target namespace; nothing already running is touched.
  - name:     optional; the workload name. A short random suffix is always
              appended. Defaults to a neutral per-kind name.
  - replicas: optional, default 1.

Give these faults a duration of at least 3m. Apply does not return until the
efficacy probe has seen the failure state, the settle wait comes out of the
fault's own lease, and a backoff state can take 30s or more to appear.`

var specTemplates = map[string]string{
	KindImageUnresolvable: `Creates a Deployment that can never start because its image does not exist.

Spec (every field optional):
  {"image": "registry.k8s.io/simian-no-such-image:v0.0.0"}

  - image: the unresolvable reference. Default points at a real registry host
           with no such repository, which yields a registry 404 rather than a
           DNS failure.
` + commonSpecNotes,

	KindContainerExitLoop: `Creates a Deployment whose container exits immediately and is restarted
until the kubelet backs off.

Spec (every field optional):
  {"exit_code": 1, "message": "fatal: initialization failed"}

  - exit_code: 1-255. Must be non-zero.
  - message:   written to stderr before exiting, so the pod logs say something
               a diagnosis can use.
` + commonSpecNotes,

	KindMemoryLimitSqueeze: `Creates a Deployment that allocates more memory than its own limit allows.

Spec (every field optional):
  {"limit_memory": "32Mi", "allocate_mb": 64}

  - limit_memory: the container's memory limit (and request).
  - allocate_mb:  how much anonymous memory it holds. Must exceed
                  limit_memory, or the container never crosses its cgroup and
                  is never OOMKilled.
` + commonSpecNotes,

	KindUnschedulable: `Creates a Deployment the scheduler cannot place.

Spec (every field optional):
  {"request_cpu": "1000"}
  {"node_selector": {"failure-domain.example.com/zone": "nowhere"}}

  - request_cpu:   CPU request no node can satisfy. The default is far beyond
                   any real machine shape on purpose: a merely large request is
                   a provisioning signal to a cluster autoscaler, which would
                   add a node and heal the fault mid-experiment.
  - node_selector: alternative mechanism; use when the scenario is about
                   placement rather than capacity. Mutually exclusive with
                   request_cpu — if set, request_cpu is ignored.
` + commonSpecNotes,
}

// checkMode validates spec.mode. Absent means synthesize.
func checkMode(spec map[string]any) error {
	switch mode := optString(spec, "mode", ModeSynthesize); mode {
	case ModeSynthesize:
		return nil
	case ModeMutate:
		return fmt.Errorf("spec.mode %q is not implemented yet — it patches an existing workload and reverts on clear, which needs the original recorded somewhere that survives a restart; use %q, which creates a new broken workload instead",
			ModeMutate, ModeSynthesize)
	default:
		return fmt.Errorf("spec.mode %q is not recognised (want %q or %q)", mode, ModeSynthesize, ModeMutate)
	}
}

// workloadName resolves the object name.
//
// The suffix catalog.KubeStateWorkloadName appends is always applied, even to
// a caller-supplied name: two faults of the same kind in one namespace would
// otherwise collide on the second Create, and a scenario that wants to name
// what it injected matches on the prefix. Deriving it from the fault UID is
// what lets the default efficacy gate write a selector for these pods before
// they exist — see that function for the rest of the reasoning.
func workloadName(kind string, spec map[string]any, faultUID string) (string, error) {
	name := catalog.KubeStateWorkloadName(kind, optString(spec, "name", ""), faultUID)
	if name == "" {
		return "", fmt.Errorf("no default workload name for kind %q", kind)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return "", fmt.Errorf("spec.name %q does not make a valid workload name: %s",
			optString(spec, "name", ""), strings.Join(errs, "; "))
	}
	return name, nil
}

// expiryAnnotation builds the annotation map for a fault of the given
// duration. A non-positive duration means the caller did not set one; rather
// than stamping a deadline that has already passed — which would make the next
// sweep delete a live fault — we stamp nothing and leave it to the in-memory
// lease.
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
