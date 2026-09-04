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

package kubestate

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The fault kinds this engine synthesizes.
//
// Each is its own ResourceKind rather than an `action` field on a single kind.
// That costs four catalog entries instead of one, and buys three things: a
// scenario's expected finding can name the kind it injected, blast-radius
// classification can diverge per kind without re-reading the spec, and the
// planner sees four distinct capabilities rather than one it has to know how
// to parameterise.
const (
	KindImageUnresolvable  = "ImageUnresolvable"
	KindContainerExitLoop  = "ContainerExitLoop"
	KindMemoryLimitSqueeze = "MemoryLimitSqueeze"
	KindUnschedulable      = "Unschedulable"
)

// Modes. `synthesize` applies a bundle that is born broken; `mutate` patches
// an existing healthy workload and reverts on clear.
//
// Only synthesize is implemented. The field is validated now rather than
// ignored so that manifests and scenario packs written today carry the mode
// explicitly, and adding mutate is a driver change rather than a change to
// every manifest that already exists.
const (
	ModeSynthesize = "synthesize"
	ModeMutate     = "mutate"
)

const (
	// containerName is deliberately generic. The workload is meant to read as
	// an ordinary broken application to whatever is diagnosing it.
	containerName = "app"

	// exitMessageEnv carries the caller's log line into the container.
	exitMessageEnv = "SIMIAN_EXIT_MESSAGE"
)

const (
	// defaultRunnerImage is the base for every kind that actually starts. Kept
	// to one small, widely-mirrored image so a synthesized bundle does not
	// itself fail to pull for reasons the scenario did not intend.
	defaultRunnerImage = "busybox:1.36"

	// defaultBadImage points at a real registry host holding no such
	// repository, rather than at a hostname that does not resolve. Both end in
	// ImagePullBackOff, but only this one produces the message an operator
	// meets in the field — a 404 from the registry, not a DNS error — and the
	// message is the evidence the agent under test has to read.
	defaultBadImage = "registry.k8s.io/simian-no-such-image:v0.0.0"

	defaultMemoryLimit      = "32Mi"
	defaultMemoryAllocateMB = 64

	// defaultUnschedulableCPU is far beyond any machine shape any cloud sells,
	// and that is the point. A merely large request (say 64 CPU) is
	// unschedulable on today's nodes but *satisfiable* by a bigger node, so a
	// cluster autoscaler or GKE Node Auto-Provisioning treats it as a
	// provisioning signal: it adds a node, the pod schedules, and the fault
	// quietly heals partway through the experiment — with a machine on the
	// bill. A request nothing can satisfy is declared unschedulable and left
	// alone.
	defaultUnschedulableCPU = "1000"

	defaultExitCode    = 1
	defaultExitMessage = "fatal: initialization failed"

	// maxReplicas caps how many broken pods one fault may create.
	//
	// A declarative-state fault is about the state, not the volume: one
	// crash-looping pod poses the same diagnosis as fifty, and fifty
	// ImagePullBackOff pods are a registry hammering the arena's own node
	// pool. The number the manifest asks for is chosen by an LLM, so an
	// unbounded field here is a blast radius nothing else in the pipeline
	// would catch — the safety stage bounds tiers and durations, not replica
	// counts.
	maxReplicas = 20
)

// The default workload name for each kind lives in pkg/catalog, alongside the
// suffix derivation, because the efficacy gate has to compute the same name
// before Apply runs. See catalog.KubeStateWorkloadName.

// podBuilder turns a validated manifest spec into the broken pod spec.
type podBuilder func(spec map[string]any) (corev1.PodSpec, error)

var builders = map[string]podBuilder{
	KindImageUnresolvable:  imageUnresolvablePod,
	KindContainerExitLoop:  containerExitLoopPod,
	KindMemoryLimitSqueeze: memoryLimitSqueezePod,
	KindUnschedulable:      unschedulablePod,
}

// Kinds returns the supported fault kinds, sorted, so the catalog and the docs
// cannot drift out of order.
func Kinds() []string {
	out := make([]string, 0, len(builders))
	for k := range builders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Supports reports whether this engine can synthesize the given kind.
func Supports(kind string) bool {
	_, ok := builders[kind]
	return ok
}

// imageUnresolvablePod: a workload that can never start because its image
// reference resolves to no manifest. Produces ErrImagePull, then
// ImagePullBackOff once the kubelet starts backing off.
func imageUnresolvablePod(spec map[string]any) (corev1.PodSpec, error) {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  containerName,
			Image: optString(spec, "image", defaultBadImage),
			// Always, so the outcome does not depend on whether some unrelated
			// thing happens to have warmed this node's image cache.
			ImagePullPolicy: corev1.PullAlways,
		}},
	}, nil
}

// containerExitLoopPod: a workload whose process exits non-zero immediately
// and is restarted until the kubelet backs off. Produces CrashLoopBackOff.
func containerExitLoopPod(spec map[string]any) (corev1.PodSpec, error) {
	code, err := optInt(spec, "exit_code", defaultExitCode)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	if code == 0 {
		return corev1.PodSpec{}, fmt.Errorf(
			"spec.exit_code must be non-zero: a container that exits 0 under restartPolicy Always still restarts into CrashLoopBackOff, but reports lastState reason Completed, which is a different diagnosis than the one this kind exists to pose")
	}
	if code < 0 || code > 255 {
		return corev1.PodSpec{}, fmt.Errorf("spec.exit_code must be between 1 and 255, got %d", code)
	}
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  containerName,
			Image: optString(spec, "image", defaultRunnerImage),
			// The message travels as an environment variable and is referenced
			// by name in the command. Interpolating caller-supplied text into
			// the shell string instead is how a scenario pack that wants a
			// specific log line becomes a command injection.
			Env: []corev1.EnvVar{{Name: exitMessageEnv, Value: optString(spec, "message", defaultExitMessage)}},
			Command: []string{"/bin/sh", "-c",
				fmt.Sprintf("echo \"$%s\" >&2; exit %d", exitMessageEnv, code)},
		}},
	}, nil
}

// memoryLimitSqueezePod: a workload whose working set is larger than its own
// memory limit. Produces OOMKilled, then CrashLoopBackOff.
//
// The allocation is anonymous memory — `tail -c` buffering a bounded read of
// /dev/zero, which has to hold the whole thing before it can print the tail of
// it. That needs no stress tool and no second image, so a synthesized bundle
// stays on one widely-cached base.
//
// It is deliberately not a write into a memory-backed emptyDir, which is the
// more obvious way to charge pages to a cgroup. A tmpfs emptyDir belongs to the
// *pod*, not the container, so its pages survive the restart: the container is
// OOM-killed, the kubelet restarts it, and the replacement's `runc init` is
// itself OOM-killed against a cgroup that is already full before any of our
// code runs. The pod then reports lastState reason StartError — measured on GKE
// at both 32Mi and 128Mi — and the OOM kill the fault is about is overwritten
// within seconds. Anonymous memory is freed with the process, so every restart
// cycle reproduces the same clean OOMKilled.
func memoryLimitSqueezePod(spec map[string]any) (corev1.PodSpec, error) {
	limitStr := optString(spec, "limit_memory", defaultMemoryLimit)
	limit, err := resource.ParseQuantity(limitStr)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("spec.limit_memory %q is not a quantity: %w", limitStr, err)
	}
	allocMB, err := optInt(spec, "allocate_mb", defaultMemoryAllocateMB)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	if allocMB <= 0 {
		return corev1.PodSpec{}, fmt.Errorf("spec.allocate_mb must be positive, got %d", allocMB)
	}
	if int64(allocMB)*1024*1024 <= limit.Value() {
		return corev1.PodSpec{}, fmt.Errorf(
			"spec.allocate_mb (%dMi) must exceed spec.limit_memory (%s): a container that stays inside its cgroup is never OOMKilled, and the fault would apply cleanly and do nothing",
			allocMB, limitStr)
	}
	bytes := int64(allocMB) * 1024 * 1024
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  containerName,
			Image: optString(spec, "image", defaultRunnerImage),
			// Trailing sleep rather than `&&`: if the allocation somehow does
			// not cross the limit, the container should idle, not exit. A
			// container that exits here would crash-loop, and a crash loop is
			// the diagnosis ContainerExitLoop poses — the fault would land as
			// the wrong fault rather than as no fault.
			Command: []string{"/bin/sh", "-c",
				fmt.Sprintf("head -c %d /dev/zero | tail -c %d >/dev/null; sleep 3600", bytes, bytes)},
			Resources: corev1.ResourceRequirements{
				// requests == limits so the scheduler reserves everything the
				// container is allowed to use. Under a smaller request the pod
				// is a candidate for eviction under node memory pressure, and
				// the fault would sometimes read as a node problem instead of
				// the container problem it is.
				Requests: corev1.ResourceList{corev1.ResourceMemory: limit},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: limit},
			},
		}},
	}, nil
}

// unschedulablePod: a workload the scheduler cannot place. Produces a Pending
// pod with a PodScheduled=False/Unschedulable condition and FailedScheduling
// events.
func unschedulablePod(spec map[string]any) (corev1.PodSpec, error) {
	ps := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:    containerName,
			Image:   optString(spec, "image", defaultRunnerImage),
			Command: []string{"/bin/sh", "-c", "sleep 3600"},
		}},
	}
	sel, err := optStringMap(spec, "node_selector")
	if err != nil {
		return corev1.PodSpec{}, err
	}
	// A node selector nothing matches is the alternative mechanism, and the
	// one to reach for when the scenario is about placement rather than
	// capacity. It is mutually exclusive with the CPU request: setting both
	// would make the FailedScheduling message name whichever the scheduler
	// happened to check first.
	if len(sel) > 0 {
		ps.NodeSelector = sel
		return ps, nil
	}
	cpuStr := optString(spec, "request_cpu", defaultUnschedulableCPU)
	cpu, err := resource.ParseQuantity(cpuStr)
	if err != nil {
		return corev1.PodSpec{}, fmt.Errorf("spec.request_cpu %q is not a quantity: %w", cpuStr, err)
	}
	if cpu.Sign() <= 0 {
		return corev1.PodSpec{}, fmt.Errorf("spec.request_cpu must be positive, got %q", cpuStr)
	}
	ps.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu},
	}
	return ps, nil
}

// newDeployment wraps a broken pod spec in the Deployment that carries it.
//
// Note where the labels go. simian.chaos/managed and the fault UID are on the
// Deployment, because ReapExpired has to be able to list what this engine
// created without help from the in-memory registry. They are deliberately not
// on the pod template: pods are what a subject under evaluation inspects, and
// a pod wearing a label with our name on it answers the question the rig is
// supposed to be asking.
func newDeployment(name, namespace string, replicas int32, kind, faultUID string, annotations map[string]string, pod corev1.PodSpec) *appsv1.Deployment {
	podLabels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedLabel:  "true",
				FaultUIDLabel: faultUID,
				KindLabel:     kind,
			},
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec:       pod,
			},
		},
	}
}

func optString(spec map[string]any, field, def string) string {
	if spec == nil {
		return def
	}
	s, ok := spec[field].(string)
	if !ok || s == "" {
		return def
	}
	return s
}

// optInt accepts the shapes a decoded JSON spec can carry a number in.
func optInt(spec map[string]any, field string, def int) (int, error) {
	if spec == nil {
		return def, nil
	}
	switch n := spec[field].(type) {
	case nil:
		return def, nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("spec.%s must be a whole number, got %v", field, n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("spec.%s must be a number, got %T", field, spec[field])
	}
}

func optStringMap(spec map[string]any, field string) (map[string]string, error) {
	if spec == nil || spec[field] == nil {
		return nil, nil
	}
	raw, ok := spec[field].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec.%s must be an object", field)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("spec.%s[%q] must be a string", field, k)
		}
		out[k] = s
	}
	return out, nil
}
