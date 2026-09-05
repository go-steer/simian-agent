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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
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
	KindJobFailure         = "JobFailure"
	KindSelectorDrift      = "SelectorDrift"
	KindUnboundClaim       = "UnboundClaim"
	KindNoOp               = "NoOp"
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

	// defaultJobFailMessage reads like a batch job that hit something it could
	// not get past, because that is the diagnosis a failed Job poses.
	defaultJobFailMessage = "fatal: migration step 3 failed: relation \"orders\" does not exist"

	// defaultBackoffLimit is low on purpose. The Job controller's retry delay
	// doubles — 10s, 20s, 40s — so every extra retry pushes the moment the
	// Job admits it has failed further out, and the fault's own lease has to
	// outlast it.
	defaultBackoffLimit = 2

	// maxBackoffLimit keeps a manifest from asking for a Job that will not
	// report failure until long after the lease expires.
	maxBackoffLimit = 6

	// defaultServicePort is what SelectorDrift's Service listens on. Nothing
	// answers on it either way — the point is a Service with no endpoints, not
	// a Service that serves.
	defaultServicePort = 8080

	// defaultDriftSuffix is appended to the workload's own name to make the
	// selector that misses it. A near-miss rather than a nonsense value: this
	// is what the fault looks like in the field, where a rename touched the
	// Deployment and not the Service.
	defaultDriftSuffix = "-v2"

	defaultClaimSize = "1Gi"

	// defaultMissingStorageClass names a class no cluster has. Spelled as a
	// plausible class name rather than as "nonexistent", so the diagnosis is
	// "this class is not installed here" — the real-world shape of the fault —
	// rather than "someone is obviously testing something".
	defaultMissingStorageClass = "fast-ssd-retain"

	claimVolumeName = "data"
	claimMountPath  = "/var/lib/data"

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

// synthesis is what a builder is handed: the identity every object in the
// bundle shares, and the spec that has already been validated for it.
type synthesis struct {
	name      string
	namespace string
	replicas  int32
	spec      map[string]any
}

// builder turns a validated synthesis into the objects to create.
//
// A slice rather than a single Deployment because half the interesting
// declarative-state faults are not about one workload. A Service pointing at
// nothing and a claim that will never bind are relationships between objects,
// and the diagnosis they pose is the relationship. The driver stamps the
// common labels and annotations, so a builder writes only what makes its own
// kind what it is.
type builder func(s synthesis) ([]runtime.Object, error)

// podBuilder turns a validated manifest spec into the broken pod spec, for
// the kinds that are one workload and nothing else.
type podBuilder func(spec map[string]any) (corev1.PodSpec, error)

var builders = map[string]builder{
	KindImageUnresolvable:  deploymentOf(imageUnresolvablePod),
	KindContainerExitLoop:  deploymentOf(containerExitLoopPod),
	KindMemoryLimitSqueeze: deploymentOf(memoryLimitSqueezePod),
	KindUnschedulable:      deploymentOf(unschedulablePod),
	KindNoOp:               deploymentOf(healthyPod),
	KindJobFailure:         jobFailureBundle,
	KindSelectorDrift:      selectorDriftBundle,
	KindUnboundClaim:       unboundClaimBundle,
}

// deploymentOf adapts a pod-spec builder to the bundle interface.
func deploymentOf(build podBuilder) builder {
	return func(s synthesis) ([]runtime.Object, error) {
		pod, err := build(s.spec)
		if err != nil {
			return nil, err
		}
		return []runtime.Object{newDeployment(s, pod)}, nil
	}
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

// healthyPod is a workload with nothing wrong with it. It is what NoOp
// synthesizes.
//
// A control that applies literally nothing would leave the subject looking at
// an empty namespace, and an empty namespace is trivially distinguishable from
// one a fault landed in — a subject could score every control correctly by
// counting objects rather than by diagnosing anything. The control has to look
// like a scenario in every respect except being broken, so it gets a workload
// too, and the workload is fine.
func healthyPod(spec map[string]any) (corev1.PodSpec, error) {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:    containerName,
			Image:   optString(spec, "image", defaultRunnerImage),
			Command: []string{"/bin/sh", "-c", "sleep 3600"},
		}},
	}, nil
}

// jobFailureBundle: a Job whose pods exit non-zero until it exhausts its
// backoff limit. Produces a Job with a Failed condition of reason
// BackoffLimitExceeded, and the failed pods that got it there.
//
// A Job rather than a Deployment because the diagnosis is different. A
// crash-looping Deployment is a workload that will keep trying forever; a Job
// past its backoff limit has given up, and whatever was waiting on it is
// waiting on something that is never going to arrive.
func jobFailureBundle(s synthesis) ([]runtime.Object, error) {
	code, err := optInt(s.spec, "exit_code", defaultExitCode)
	if err != nil {
		return nil, err
	}
	if code <= 0 || code > 255 {
		// Zero is refused rather than defaulted: a Job whose pod exits 0
		// succeeds, and the fault would apply cleanly and produce a healthy
		// namespace.
		return nil, fmt.Errorf("spec.exit_code must be between 1 and 255, got %d", code)
	}
	backoff, err := optInt(s.spec, "backoff_limit", defaultBackoffLimit)
	if err != nil {
		return nil, err
	}
	if backoff < 0 || backoff > maxBackoffLimit {
		// Bounded because the retry backoff doubles: the pod waits 10s, 20s,
		// 40s and on, so a limit of 10 is over three hours before the Job
		// reports the failure the fault is about, and the lease would expire
		// first.
		return nil, fmt.Errorf("spec.backoff_limit must be between 0 and %d, got %d", maxBackoffLimit, backoff)
	}

	limit := int32(backoff)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &limit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": s.name}},
				Spec: corev1.PodSpec{
					// Never, not OnFailure. Under OnFailure the kubelet
					// restarts the container in place and the Job's own
					// backoff never advances, so it never reaches
					// BackoffLimitExceeded — it just crash-loops, which is
					// the diagnosis ContainerExitLoop already poses.
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  containerName,
						Image: optString(s.spec, "image", defaultRunnerImage),
						Env: []corev1.EnvVar{{
							Name:  exitMessageEnv,
							Value: optString(s.spec, "message", defaultJobFailMessage),
						}},
						Command: []string{"/bin/sh", "-c",
							fmt.Sprintf("echo \"$%s\" >&2; exit %d", exitMessageEnv, code)},
					}},
				},
			},
		},
	}
	return []runtime.Object{job}, nil
}

// selectorDriftBundle: a healthy workload, and a Service whose selector does
// not match it. Produces a Service with no endpoints in front of pods that are
// running and Ready.
//
// This is the shape that catches an agent grading `kubectl get pods`. Every
// pod is Running, the Deployment is Available, nothing has restarted, and the
// service is black-holing every request that reaches it. The fault is in the
// relationship between two objects and is invisible in either one alone.
func selectorDriftBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := healthyPod(s.spec)
	if err != nil {
		return nil, err
	}
	port, err := optInt(s.spec, "port", defaultServicePort)
	if err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("spec.port must be between 1 and 65535, got %d", port)
	}
	drifted := optString(s.spec, "selector_value", s.name+defaultDriftSuffix)
	if drifted == s.name {
		return nil, fmt.Errorf(
			"spec.selector_value %q is the workload's own label value: the Service would select the pods after all, and the fault would apply cleanly and do nothing", drifted)
	}
	if errs := validation.IsValidLabelValue(drifted); len(errs) > 0 {
		return nil, fmt.Errorf("spec.selector_value %q is not a valid label value: %s", drifted, strings.Join(errs, "; "))
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		Spec: corev1.ServiceSpec{
			// The drift itself: the pods are labelled app=<name>, and this
			// asks for something else. One character off is what this looks
			// like in the field — a rename that updated the Deployment and
			// not the Service.
			Selector: map[string]string{"app": drifted},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       int32(port),
				TargetPort: intstr.FromInt32(int32(port)),
			}},
		},
	}
	return []runtime.Object{newDeployment(s, pod), svc}, nil
}

// unboundClaimBundle: a PersistentVolumeClaim on a StorageClass that does not
// exist, and a workload that mounts it. Produces a Pending claim and a pod the
// scheduler will not place.
//
// The claim is what is wrong, and the pod is where it shows. A diagnosis that
// stops at "the pod is Pending" has found the symptom; the answer is one
// object further down.
func unboundClaimBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := healthyPod(s.spec)
	if err != nil {
		return nil, err
	}
	sizeStr := optString(s.spec, "size", defaultClaimSize)
	size, err := resource.ParseQuantity(sizeStr)
	if err != nil {
		return nil, fmt.Errorf("spec.size %q is not a quantity: %w", sizeStr, err)
	}
	if size.Sign() <= 0 {
		return nil, fmt.Errorf("spec.size must be positive, got %q", sizeStr)
	}
	class := optString(s.spec, "storage_class", defaultMissingStorageClass)
	if errs := validation.IsDNS1123Subdomain(class); len(errs) > 0 {
		return nil, fmt.Errorf("spec.storage_class %q is not a valid name: %s", class, strings.Join(errs, "; "))
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			// Named explicitly rather than left empty. An empty
			// storageClassName means "the cluster default", which on a managed
			// cluster is a real provisioner that would bind the claim within
			// seconds and heal the fault.
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}

	pod.Volumes = []corev1.Volume{{
		Name: claimVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.name},
		},
	}}
	pod.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      claimVolumeName,
		MountPath: claimMountPath,
	}}

	// The claim first: a Deployment whose pod references a claim that does not
	// exist yet reports a different failure — FailedMount rather than an
	// unschedulable pod — and the gate reads the second one.
	return []runtime.Object{pvc, newDeployment(s, pod)}, nil
}

// newDeployment wraps a pod spec in the Deployment that carries it.
//
// Only `app` goes on here. simian.chaos/managed, the fault UID and the kind
// are stamped by the driver onto every object in the bundle, because
// ReapExpired has to be able to list what this engine created without help
// from the in-memory registry. None of them reach the pod template: pods are
// what a subject under evaluation inspects, and a pod wearing a label with our
// name on it answers the question the rig is supposed to be asking.
func newDeployment(s synthesis, pod corev1.PodSpec) *appsv1.Deployment {
	podLabels := map[string]string{"app": s.name}
	replicas := s.replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
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
