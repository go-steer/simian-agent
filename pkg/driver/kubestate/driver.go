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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// BundleLabel groups the objects one fault synthesized. A fault is not always
// one object — a Service pointing at nothing needs the workload it misses, a
// claim that never binds needs something to mount it — and this is what lets
// Clear find all of them from the engineUID alone.
const BundleLabel = "simian.chaos/bundle"

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

	// RolloutSettle bounds how long a finisher waits for the cluster to reach
	// the state it needs before it can be wedged. Zero means
	// rolloutSettleTimeout.
	//
	// Overridable because the wait is a real wait against a real controller,
	// and the fake clientset unit tests run against has none. Without this a
	// test of RolloutStuck would either sleep out the full timeout or have to
	// skip the finisher, and skipping it would leave the one step this kind
	// cannot be built without untested.
	RolloutSettle time.Duration
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

func (d *Driver) rolloutSettle() time.Duration {
	if d.RolloutSettle > 0 {
		return d.RolloutSettle
	}
	return rolloutSettleTimeout
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
	// The default comes from the catalog rather than from a literal here,
	// because a kind whose gate asserts on the replica count has to be able to
	// compute the same number before Apply runs. See
	// catalog.KubeStateDefaultReplicas.
	replicas, err := optInt(m.Spec, "replicas", catalog.KubeStateDefaultReplicas(m.ResourceKind))
	if err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}
	if replicas < 1 || replicas > maxReplicas {
		return "", fmt.Errorf("kube-state apply: spec.replicas must be between 1 and %d, got %d", maxReplicas, replicas)
	}
	now := d.now()
	s := synthesis{
		name:      name,
		namespace: ns,
		replicas:  int32(replicas),
		spec:      m.Spec,
		now:       now,
		settle:    d.rolloutSettle(),
	}
	objs, err := build(s)
	if err != nil {
		return "", fmt.Errorf("kube-state apply: %w", err)
	}

	labels := bundleLabels(name, m.ResourceKind, m.UID)
	annotations := expiryAnnotation(now, m.Duration)
	for i, obj := range objs {
		if err := stamp(obj, labels, annotations); err != nil {
			return "", fmt.Errorf("kube-state apply: %w", err)
		}
		if err := createObject(ctx, d.clientset, ns, obj); err != nil {
			// Roll back what did land. A failed Apply is never leased, so
			// nothing will ever call Clear for it, and a half-applied bundle
			// left in the arena is a fault nobody is tracking and nobody will
			// take out.
			if cerr := d.clearBundle(ctx, ns, name); cerr != nil {
				return "", fmt.Errorf("kube-state apply: create %s %s/%s: %w (and rolling back the %d object(s) already created: %v)",
					describeObject(obj), ns, name, err, i, cerr)
			}
			return "", fmt.Errorf("kube-state apply: create %s %s/%s: %w", describeObject(obj), ns, name, err)
		}
	}

	// A kind whose failure state is arrived at rather than created gets its
	// second step here — see finisher. Rolled back the same way a failed create
	// is: a bundle that got half way to being a fault is one nothing will ever
	// clear, because a failed Apply is never leased.
	if finish, ok := finishers[m.ResourceKind]; ok {
		if err := finish(ctx, d.clientset, s); err != nil {
			if cerr := d.clearBundle(ctx, ns, name); cerr != nil {
				return "", fmt.Errorf("kube-state apply: %w (and rolling back the bundle: %v)", err, cerr)
			}
			return "", fmt.Errorf("kube-state apply: %w", err)
		}
	}
	return engineUID(ns, name), nil
}

// bundleLabels are stamped onto every object a fault synthesizes.
//
// BundleLabel is what makes Clear possible without recording anything: the
// engineUID names the bundle, and the label is how the objects in it are found
// again. Deleting by name across every type this engine creates would be
// simpler and is not what this does — the label is a promise that Simian only
// removes objects it put there.
func bundleLabels(name, kind, faultUID string) map[string]string {
	return map[string]string{
		ManagedLabel:  "true",
		BundleLabel:   name,
		FaultUIDLabel: faultUID,
		KindLabel:     kind,
	}
}

// stamp merges the bundle's shared labels and annotations onto one object,
// leaving whatever the builder set in place.
func stamp(obj runtime.Object, labels, annotations map[string]string) error {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("cannot read metadata of %T: %w", obj, err)
	}
	existing := acc.GetLabels()
	if existing == nil {
		existing = map[string]string{}
	}
	for k, v := range labels {
		existing[k] = v
	}
	acc.SetLabels(existing)

	if len(annotations) == 0 {
		return nil
	}
	ann := acc.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	for k, v := range annotations {
		ann[k] = v
	}
	acc.SetAnnotations(ann)
	return nil
}

// Clear implements ChaosDriver. Idempotent — an object already gone is
// treated as success.
//
// Deleting the workload garbage-collects its ReplicaSet and pods, so a
// synthesized fault leaves nothing behind. That is the whole recovery story
// for synthesize mode: nothing that existed before the fault was touched, so
// there is nothing to restore.
func (d *Driver) Clear(ctx context.Context, engineUIDStr string) error {
	ns, name, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		return err
	}
	if err := d.clearBundle(ctx, ns, name); err != nil {
		return fmt.Errorf("kube-state clear %s/%s: %w", ns, name, err)
	}
	return nil
}

// clearBundle removes every object wearing this bundle's label.
//
// It asks each type in turn rather than deleting by name, so that the only
// objects it can possibly remove are ones this engine labelled. A bundle with
// nothing of a given type in it costs one empty list, which is the price of
// not needing to have recorded what the bundle contained.
func (d *Driver) clearBundle(ctx context.Context, ns, name string) error {
	selector := ManagedLabel + "=true," + BundleLabel + "=" + name
	var errs []error
	for _, r := range managedResources {
		found, err := r.list(ctx, d.clientset, ns, selector)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("list %s: %w", r.plural, err))
			continue
		}
		for _, obj := range found {
			if err := r.del(ctx, d.clientset, ns, obj.Name); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete %s/%s: %w", r.plural, obj.Name, err))
			}
		}
	}
	return errors.Join(errs...)
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
		seen    = map[string]bool{}
		errs    []error
	)
	for _, ns := range namespaces {
		for _, r := range managedResources {
			found, err := r.list(ctx, d.clientset, ns, ManagedLabel+"=true")
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				errs = append(errs, fmt.Errorf("kube-state reap: list %s in %s: %w", r.plural, ns, err))
				continue
			}
			for _, obj := range found {
				expiry, ok := parseExpiry(obj.Annotations)
				if !ok || !expiry.Before(now) {
					continue
				}
				if err := r.del(ctx, d.clientset, ns, obj.Name); err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("kube-state reap: delete %s %s/%s: %w", r.plural, ns, obj.Name, err))
					continue
				}
				// Reported once per bundle, not once per object. The caller
				// clears a lease per engineUID, and a bundle of three expired
				// objects is one fault that has ended, not three.
				uid := engineUID(ns, bundleNameOf(obj))
				if seen[uid] {
					continue
				}
				seen[uid] = true
				cleared = append(cleared, uid)
			}
		}
	}
	return cleared, errors.Join(errs...)
}

// bundleNameOf reads the bundle an object belongs to. Falls back to the
// object's own name, which is what an object written by an older Simian —
// before bundles existed, when every fault was one Deployment — carries.
func bundleNameOf(obj metav1.ObjectMeta) string {
	if b := obj.Labels[BundleLabel]; b != "" {
		return b
	}
	return obj.Name
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
	KindJobFailure:         "Synthesize a Job whose pods exit non-zero until it exhausts its backoff limit, producing a Failed Job with reason BackoffLimitExceeded.",
	KindSelectorDrift:      "Synthesize a healthy workload behind a Service whose selector does not match it, producing an endpointless Service in front of Running pods.",
	KindBackendCrashLoop:   "Synthesize a crash-looping workload behind a Service that selects it correctly, producing a Service whose endpoints are all not ready. The cause and its consequence are separate objects in separate states.",
	KindUnboundClaim:       "Synthesize a PersistentVolumeClaim on a StorageClass that does not exist, and a workload that mounts it, producing a Pending claim and an unschedulable pod.",
	KindDependencyStall:    "Synthesize a workload that serves normally and logs upstream failures. Every API-server check is clean — Ready, Available, endpointed — and the only evidence is in the pod log.",
	KindPDBGridlock:        "Synthesize a healthy workload under a PodDisruptionBudget with no headroom, producing a namespace where nothing is failing and no pod can be evicted — node drains and autoscaler scale-downs hang indefinitely.",
	KindRolloutStuck:       "Synthesize a Deployment, let it become fully available, then roll out a revision that can never become ready. The previous revision keeps serving every request, so nothing alerts and the deploy silently never landed.",
	KindCertExpiry:         "Synthesize a TLS Secret whose certificate expires within hours (or has already expired) and a workload that mounts it. Nothing is failing yet; the failure is scheduled.",
	KindNoOp:               "Synthesize a workload with nothing wrong with it. The control case: a namespace that looks like every other scenario and holds no fault.",
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
  - replicas: optional, default 1 — except RolloutStuck, which defaults to 2
              so "the old revision is still serving" means more than one pod.

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
  {"pending_dwell": "5m30s"}

  - request_cpu:   CPU request no node can satisfy. The default is far beyond
                   any real machine shape on purpose: a merely large request is
                   a provisioning signal to a cluster autoscaler, which would
                   add a node and heal the fault mid-experiment.
  - node_selector: alternative mechanism; use when the scenario is about
                   placement rather than capacity. Mutually exclusive with
                   request_cpu — if set, request_cpu is ignored.
  - pending_dwell: how long the pod must stay Pending before the efficacy gate
                   hands the scenario over, default 90s, capped at 10m. This
                   one is a gate knob rather than a workload knob — the driver
                   does not read it — and it is here because it is the only
                   kind whose fault is an age: two seconds of Pending is a busy
                   scheduler. Raise it for a subject with a longer grace period
                   than Simian's; the whole hold comes out of the fault's lease.
` + commonSpecNotes,

	KindJobFailure: `Creates a Job whose pods exit non-zero until it gives up.

Spec (every field optional):
  {"exit_code": 1, "backoff_limit": 2, "message": "fatal: migration step 3 failed"}

  - exit_code:     1-255. Must be non-zero, or the Job succeeds.
  - backoff_limit: 0-6. Retries before the Job reports Failed. The delay
                   between retries doubles, so each extra retry pushes the
                   failure further out and the fault's lease has to outlast it.
  - message:       written to stderr by each attempt.
` + commonSpecNotes,

	KindSelectorDrift: `Creates a healthy Deployment and a Service that does not select it.

Spec (every field optional):
  {"selector_value": "checkout-v2", "port": 8080}

  - selector_value: what the Service asks for in its app selector. Defaults to
                    the workload's own name with "-v2" appended — the shape of
                    a rename that updated one object and not the other.
                    Refused if it equals the workload's own label value.
  - port:           the port the Service publishes. Nothing answers on it
                    either way; the fault is the absence of endpoints.

Every pod is Running and Ready and the Deployment is Available. The fault is
in the relationship between the two objects and is invisible in either alone.
` + commonSpecNotes,

	KindBackendCrashLoop: `Creates a Deployment whose container exits immediately, and a Service
that selects it correctly.

Spec (every field optional):
  {"exit_code": 1, "message": "fatal: initialization failed", "port": 8080}

  - exit_code: 1-255. Must be non-zero.
  - message:   written to stderr before exiting.
  - port:      the port the Service publishes and the pods declare.

Defaults to 2 replicas, so "the Service has no healthy backend" is a statement
about a set rather than about one pod.

The Service is written correctly and has nothing ready to point at. Presents
identically to SelectorDrift — a Service that is not serving — and is a
different fix: the selector is right and the application is broken. The root
cause and its consequence are separate objects, which is what makes this the
kind a scoring run uses to ask whether a report stopped at the symptom.
` + commonSpecNotes,

	KindUnboundClaim: `Creates a PersistentVolumeClaim that can never bind, and a Deployment
that mounts it.

Spec (every field optional):
  {"storage_class": "fast-ssd-retain", "size": "1Gi"}

  - storage_class: a class the cluster does not have. Named explicitly rather
                   than left empty, because an empty class means the cluster
                   default, which on a managed cluster binds within seconds
                   and heals the fault.
  - size:          the requested capacity.

The claim is what is wrong and the pod is where it shows. A diagnosis that
stops at "the pod is Pending" has found the symptom, not the cause.
` + commonSpecNotes,

	KindDependencyStall: `Creates a Deployment that serves normally and writes upstream-failure
lines to its log, behind a Service that selects it correctly.

Spec (every field optional):
  {"message": "level=error msg=\"upstream request failed\" upstream=payments-api",
   "interval_seconds": 10, "port": 8080}

  - message:          the line written to stderr. Must be a single line: the
                      efficacy gate matches it against one line of the log.
  - interval_seconds: 1-60, how often it repeats.
  - port:             the port the workload serves and the Service publishes.

Nothing in the API server is wrong. The Deployment is Available, every pod is
Running and Ready, the Service has ready endpoints, nothing has restarted and
no event has fired. Use this when the scenario is about whether the subject
reads logs at all — a diagnosis built from ` + "`kubectl get`" + ` alone reports the
namespace healthy.
` + commonSpecNotes,

	KindPDBGridlock: `Creates a healthy Deployment and a PodDisruptionBudget over it that
permits no disruptions at all.

Spec (every field optional):
  {"min_available": 1}

  - min_available: how many pods the budget requires to stay up. Defaults to
                   the replica count, which leaves exactly zero headroom.
                   Refused if it is below the replica count, because then the
                   budget allows an eviction and blocks nothing.

Nothing is failing. Every pod is Ready and the Deployment is Available; the
fault only shows when something tries to move a pod, and then it never
finishes. Use this when the scenario is about a drain that hangs, a node pool
upgrade that stalls on its first node, or an autoscaler that will not scale
down.

Be aware of what this does to a shared cluster: the budget is namespaced and
covers only the fault's own pods, but for as long as the fault lasts, the node
hosting those pods cannot be drained.
` + commonSpecNotes,

	KindRolloutStuck: `Creates a Deployment, waits for it to be fully available, then updates it
to a revision that can never become ready.

Spec (every field optional):
  {"progress_deadline_seconds": 60, "broken_image": "busybox:1.37",
   "message": "fatal: config: unknown key \"featureFlags.checkoutV2\""}

  - progress_deadline_seconds: 30-600. How long before the Deployment reports
                               Progressing=False/ProgressDeadlineExceeded. Low
                               by default: the Kubernetes default of 600 is ten
                               minutes of the fault's lease spent waiting.
  - broken_image:              what the wedged revision rolls to. A different
                               tag of the same base by default, which is what a
                               bad deploy looks like.
  - message:                   written to stderr by the new revision's pods
                               before they exit. Single line.

Apply takes longer for this kind than for any other: it does not return until
the healthy revision is available and the wedge is applied, which is up to two
minutes before the efficacy gate even begins. Give it a duration of 10m or
more, and note that the gate itself then waits out
progress_deadline_seconds.

The rollout is stuck with maxUnavailable 0, so the previous revision keeps
serving at full capacity. Nothing alerts. The Deployment is Available, the
error rate is flat, and the only symptom is that a deploy everyone believes
shipped is still not running.
` + commonSpecNotes,

	KindCertExpiry: `Creates a TLS Secret holding a self-signed certificate that expires soon,
and a Deployment that mounts it.

Spec (every field optional):
  {"expires_in_hours": 48, "common_name": "api.internal"}

  - expires_in_hours: -168 to 8760. Negative means already expired, which is a
                      real and different diagnosis — "it broke last Tuesday"
                      rather than "it breaks on Thursday".
  - common_name:      the certificate subject, also used as its DNS SAN.

Nothing is failing and nothing will fail during the experiment. The pods are
Ready, the Secret is well-formed, and every API-server check is clean. The
fault is in a field nobody reads until it is too late: the certificate's
notAfter. Use this when the scenario is about whether the subject inspects
what it finds or only lists it.
` + commonSpecNotes,

	KindNoOp: `Creates a Deployment with nothing wrong with it.

Spec (every field optional):
  {}

The control case. It exists so that "no fault" is a scenario the subject has
to reach by diagnosis rather than by noticing an empty namespace: a control
that applied nothing could be scored correctly by counting objects. A subject
that reports a finding here has hallucinated it.
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
