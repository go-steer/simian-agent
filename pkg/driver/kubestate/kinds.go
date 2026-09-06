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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/go-steer/simian-agent/pkg/catalog"
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
	KindBackendCrashLoop   = "BackendCrashLoop"
	KindUnboundClaim       = "UnboundClaim"
	KindDependencyStall    = "DependencyStall"
	KindPDBGridlock        = "PDBGridlock"
	KindRolloutStuck       = "RolloutStuck"
	KindCertExpiry         = "CertExpiry"
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

	// defaultServicePort is what the Service listens on for every kind that
	// has one. Nothing answers on it in SelectorDrift or BackendCrashLoop —
	// the point is a Service with nothing serving behind it, not a Service
	// that serves.
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

	// stallMessageEnv carries the caller's dependency-error line into the
	// container, for the same reason exitMessageEnv does: interpolating
	// caller-supplied text into a shell string is how a scenario pack that
	// wants a specific log line becomes a command injection.
	stallMessageEnv = "SIMIAN_STALL_MESSAGE"

	// defaultStallSeconds is how often the workload repeats its error line.
	// Frequent enough that a gate polling every two seconds finds it inside its
	// first few polls, slow enough to read like an application retrying an
	// upstream rather than a log generator.
	defaultStallSeconds = 10

	// maxStallSeconds keeps the interval inside the gate's own timeout. A
	// workload that logs once a minute would make the fault land and the gate
	// give up, which reads as a fault that did not land.
	maxStallSeconds = 60

	// stallServeRoot is where the synthesized workload's one static page lives.
	// Under /tmp so the container needs no writable image layer and no volume.
	stallServeRoot = "/tmp/www"

	// defaultBrokenRolloutImage is what RolloutStuck rolls *to*. A different
	// tag of the same base, because a bad deploy in the field is almost always
	// a new tag of something that was working, and because the image reference
	// changing is what makes the wedged revision legible in `kubectl rollout
	// history` rather than a mystery diff.
	//
	// The image is real and pullable. If it somehow is not, the rollout stalls
	// on ImagePullBackOff instead of CrashLoopBackOff and the gate still passes
	// — the kind is about the rollout, not about how the new pod fails.
	defaultBrokenRolloutImage = "busybox:1.37"

	// defaultRolloutMessage reads like a deploy that shipped a config the new
	// binary could not load, which is the most common way a rollout wedges
	// while the old revision keeps serving.
	defaultRolloutMessage = "fatal: config: unknown key \"featureFlags.checkoutV2\""

	// defaultProgressDeadline is how long the Deployment controller waits
	// before declaring the rollout failed.
	//
	// Kubernetes defaults this to 600s. That is right for production and wrong
	// here: it is ten minutes of the fault's own lease spent waiting for a
	// condition the fault has already caused, and the efficacy gate would time
	// out long before. Sixty seconds is long enough that a slow image pull does
	// not trip it and short enough to fit inside a scenario.
	defaultProgressDeadline = 60

	// progressDeadlineBounds keep the deadline inside the gate's patience. The
	// floor is above the time a pull-and-crash cycle takes, so a healthy
	// rollout is not declared failed for being slow.
	minProgressDeadline = 30
	maxProgressDeadline = 600

	// defaultMinReadySeconds is how long a pod has to stay Ready before the
	// Deployment counts it as available. See rolloutStuckBundle for why the
	// field is set at all; ten seconds is comfortably longer than the time a
	// container takes to exit and comfortably shorter than minProgressDeadline,
	// which the healthy revision has to clear twice over.
	defaultMinReadySeconds = 10

	// rolloutSettleTimeout is how long the finisher waits for the first
	// revision to be fully available before wedging the second.
	//
	// The wait is not optional. Patching a Deployment whose first revision has
	// not finished rolling out produces one revision that never completed
	// rather than a good revision blocked behind a bad one, and the gate's
	// second assertion — every replica of the previous revision still available
	// — would be false against a fault that "landed".
	rolloutSettleTimeout = 120 * time.Second
	rolloutPollInterval  = 2 * time.Second

	// defaultCommonName is the subject and DNS SAN of a CertExpiry
	// certificate. An internal-sounding name, because a certificate that
	// expired on an internal service is the version of this fault that actually
	// pages someone.
	defaultCommonName = "api.internal"

	certVolumeName = "tls"
	certMountPath  = "/etc/tls"

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

	// now is the driver's clock, threaded through so a kind that bakes a
	// timestamp into what it creates — CertExpiry, whose whole subject is a
	// notAfter — can be tested against a frozen one.
	now time.Time

	// settle bounds how long a finisher waits for the cluster before giving
	// up. Builders ignore it; see finisher.
	settle time.Duration
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
	KindBackendCrashLoop:   backendCrashLoopBundle,
	KindUnboundClaim:       unboundClaimBundle,
	KindDependencyStall:    dependencyStallBundle,
	KindPDBGridlock:        pdbGridlockBundle,
	KindRolloutStuck:       rolloutStuckBundle,
	KindCertExpiry:         certExpiryBundle,
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
	port, err := servicePort(s.spec)
	if err != nil {
		return nil, err
	}
	drifted := optString(s.spec, "selector_value", s.name+defaultDriftSuffix)
	if drifted == s.name {
		return nil, fmt.Errorf(
			"spec.selector_value %q is the workload's own label value: the Service would select the pods after all, and the fault would apply cleanly and do nothing", drifted)
	}
	if errs := validation.IsValidLabelValue(drifted); len(errs) > 0 {
		return nil, fmt.Errorf("spec.selector_value %q is not a valid label value: %s", drifted, strings.Join(errs, "; "))
	}

	// The drift itself: the pods are labelled app=<name>, and the Service asks
	// for something else. One character off is what this looks like in the
	// field — a rename that updated the Deployment and not the Service.
	svc := newService(s, port, drifted)
	return []runtime.Object{newDeployment(s, pod), svc}, nil
}

// backendCrashLoopBundle: a crash-looping workload behind a Service that
// selects it correctly. Produces a Service whose only endpoints are not ready.
//
// The inverse of SelectorDrift in what is wrong and the same in what it costs
// to diagnose. There the Service is broken and the pods are fine; here the
// Service is written correctly and has nothing healthy to point at. Both
// present as "this Service is not serving", and telling them apart is the
// whole exercise: one is fixed by editing a selector, the other by fixing an
// application that will not start.
//
// This is the only kind whose bundle has a cause and a consequence that are
// both visible as separate objects in separate states, which is what makes it
// the one a scoring run can use to ask whether a subject reported the root or
// only the symptom. A report that names the Service and stops has described
// what a user would notice and said nothing about why.
func backendCrashLoopBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := containerExitLoopPod(s.spec)
	if err != nil {
		return nil, err
	}
	port, err := servicePort(s.spec)
	if err != nil {
		return nil, err
	}

	c := &pod.Containers[0]
	c.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port}}
	// A readiness probe on a container that exits in under a second looks
	// redundant, and is not. Without one the kubelet calls a container Ready
	// the moment it is Running, so every restart flickers the pod Ready and
	// the endpointslice controller copies that into `conditions.ready`.
	// Measured on GKE 1.36 against exactly this bundle minus the probe: the
	// endpoints read ready on 2 of the first 45 polls, both inside the first
	// 90 seconds, before the kubelet's backoff grew long enough to hide it.
	//
	// The efficacy gate would survive that — it polls until it sees the state
	// it wants, so a flicker costs it one poll. What does not survive it is the
	// subject. This kind's whole symptom is a Service with no healthy backend,
	// and a symptom that is intermittently absent is one a correct diagnosis
	// can be graded wrong against, in the first ninety seconds, which is
	// exactly when something triaging the namespace is looking.
	//
	// It is also what the fault would look like anyway. A readiness probe is
	// the normal case for a service, and it is what keeps a crashing backend
	// out of rotation rather than black-holing requests into it.
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/false"}},
		},
		PeriodSeconds:    5,
		FailureThreshold: 1,
	}

	// Correct, and that is the point: the selector matches, so the missing
	// backends are a consequence of the pods rather than a second mistake.
	svc := newService(s, port, s.name)
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

// dependencyStallBundle: a workload that serves, and complains. It answers its
// readiness probe on every poll, sits behind a Service that selects it
// correctly, and writes an upstream-failure line to its log every few seconds.
//
// This is the hardest kind in the engine and the one worth the most. Every
// check an agent runs against the API server comes back clean: the Deployment
// is Available, every pod is Running and Ready, the Service has endpoints,
// nothing has restarted, no event has fired, and there is no reason token
// anywhere to grep for. The only evidence is what the application says about
// itself, so the fault separates a subject that diagnoses from one that
// transcribes `kubectl get` — which is the whole point of having a control and
// a scoring rig at all.
//
// The workload really serves rather than merely lacking a readiness probe. A
// pod that is Ready only because nothing checks would be found by a subject
// that dialled the Service and got a connection refused, and it would be found
// for the wrong reason: the diagnosis this kind poses is "the thing behind it
// is broken", not "this is broken".
func dependencyStallBundle(s synthesis) ([]runtime.Object, error) {
	port, err := servicePort(s.spec)
	if err != nil {
		return nil, err
	}
	every, err := optInt(s.spec, "interval_seconds", defaultStallSeconds)
	if err != nil {
		return nil, err
	}
	if every < 1 || every > maxStallSeconds {
		return nil, fmt.Errorf("spec.interval_seconds must be between 1 and %d, got %d", maxStallSeconds, every)
	}
	// Resolved through the catalog because the efficacy gate greps for exactly
	// this string and has to compute it from the same spec, before the pod
	// exists. See catalog.KubeStateStallMessage.
	msg := catalog.KubeStateStallMessage(optString(s.spec, "message", ""))
	if strings.ContainsAny(msg, "\n\r") {
		// The gate matches a substring of one log line. A message carrying a
		// newline would be written as several, and the gate would look for a
		// string that appears nowhere — the fault would land and be reported as
		// inert.
		return nil, fmt.Errorf("spec.message must be a single line: the efficacy gate matches it against one line of the log")
	}

	pod := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  containerName,
			Image: optString(s.spec, "image", defaultRunnerImage),
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:   []corev1.EnvVar{{Name: stallMessageEnv, Value: msg}},
			// busybox httpd daemonizes, so the `&&` chain reaches the loop and
			// the loop is what keeps PID 1 alive. If httpd cannot bind, the
			// chain stops there and the container exits: the fault fails its own
			// readiness gate rather than landing as a workload that only looks
			// healthy.
			Command: []string{"/bin/sh", "-c", fmt.Sprintf(
				"mkdir -p %[1]s && echo ok > %[1]s/index.html && httpd -p %[2]d -h %[1]s && "+
					"while :; do echo \"$%[3]s\" >&2; sleep %[4]d; done",
				stallServeRoot, port, stallMessageEnv, every)},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(port)},
				},
				PeriodSeconds:    2,
				TimeoutSeconds:   2,
				FailureThreshold: 3,
			},
		}},
	}

	// Correct, deliberately, and the inverse of SelectorDrift. The Service
	// having *ready* endpoints is half of what makes this fault what it is,
	// and the gate asserts it.
	svc := newService(s, port, s.name)
	return []runtime.Object{newDeployment(s, pod), svc}, nil
}

// pdbGridlockBundle: a healthy workload, and a PodDisruptionBudget over it
// with no headroom. Produces a namespace where every pod is Ready and no pod
// may be evicted.
//
// Nothing is failing, which is the point. The fault is invisible until someone
// tries to move a pod — a node drain that hangs forever, a cluster autoscaler
// that cannot remove an underused node, a rolling node upgrade that stops on
// the first node and stays there. Diagnosing it means reading a budget nobody
// looks at until it bites, and the operator who wrote it usually meant to
// protect availability rather than to freeze the cluster.
func pdbGridlockBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := healthyPod(s.spec)
	if err != nil {
		return nil, err
	}
	// Defaults to the replica count, which is the smallest value that leaves
	// no headroom: with minAvailable == replicas the budget permits a
	// disruption only if a pod is already down, and nothing is.
	minAvail, err := optInt(s.spec, "min_available", int(s.replicas))
	if err != nil {
		return nil, err
	}
	if minAvail < 1 {
		return nil, fmt.Errorf(
			"spec.min_available must be at least 1, got %d: a budget of 0 permits every eviction, and the fault would apply cleanly and block nothing", minAvail)
	}
	if minAvail < int(s.replicas) {
		return nil, fmt.Errorf(
			"spec.min_available (%d) must be at least spec.replicas (%d): with headroom the budget allows %d disruption(s), the pods can be evicted, and the fault would apply cleanly and block nothing",
			minAvail, s.replicas, int(s.replicas)-minAvail)
	}
	// A budget above the replica count is legal and useful — it is how a
	// gridlock survives someone scaling the workload up — but it is a pod count,
	// and there is no reading of "protect four billion pods" that is not a typo.
	// The ceiling is maxReplicas for the same reason that caps replicas.
	if minAvail > maxReplicas {
		return nil, fmt.Errorf("spec.min_available must be at most %d, got %d", maxReplicas, minAvail)
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			// MinAvailable rather than MaxUnavailable, because that is how this
			// gets written in the field: someone protecting a service says how
			// much of it must survive, and on a small Deployment that number is
			// the whole thing.
			MinAvailable: ptr(intstr.FromInt32(int32(minAvail))),
			// Selects only the fault's own pods. A budget over a broader
			// selector would block eviction of workloads Simian did not create,
			// which is outside the blast radius this kind is classified at.
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.name}},
		},
	}

	// The workload first. A budget created over pods that do not exist yet
	// reports disruptionsAllowed 0 for the vacuous reason — there is nothing to
	// disrupt — and the ordering keeps the window where that is the reading as
	// short as the API server can make it.
	return []runtime.Object{newDeployment(s, pod), pdb}, nil
}

// rolloutStuckBundle: a Deployment that is serving its previous revision and
// cannot finish rolling out its next one.
//
// Only the healthy revision is created here. The wedge is applied afterwards by
// the finisher, because a stuck rollout is a relationship between two revisions
// and one Create cannot produce it — see wedgeRollout.
//
// maxUnavailable 0 is what makes the fault what it is: the controller will not
// take an old pod down until a new one is ready, and no new one ever will be,
// so the old revision serves every request for as long as the fault lasts.
// Nothing pages. The Deployment is Available, the Service has endpoints, error
// rates are flat, and the only thing wrong is that a deploy from an hour ago
// never landed and everyone believes it did.
func rolloutStuckBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := healthyPod(s.spec)
	if err != nil {
		return nil, err
	}
	deadline, err := optInt(s.spec, "progress_deadline_seconds", defaultProgressDeadline)
	if err != nil {
		return nil, err
	}
	if deadline < minProgressDeadline || deadline > maxProgressDeadline {
		return nil, fmt.Errorf("spec.progress_deadline_seconds must be between %d and %d, got %d",
			minProgressDeadline, maxProgressDeadline, deadline)
	}
	// Validated here rather than in the finisher so a bad spec is refused
	// before anything is created, like every other kind's.
	if _, err := brokenRolloutContainer(s.spec); err != nil {
		return nil, err
	}

	dep := newDeployment(s, pod)
	dep.Spec.ProgressDeadlineSeconds = ptr(int32(deadline))
	// The backstop under the broken revision's readiness probe, and the reason
	// this kind does not take the arena down.
	//
	// A pod with no readiness probe is Ready the moment its container is
	// running, and a container that exits after 200ms is running for 200ms. On
	// the first GKE run that was enough: the kubelet reported both broken pods
	// Ready, the Deployment controller counted the new ReplicaSet available,
	// wrote Progressing=True/NewReplicaSetAvailable, scaled the old revision to
	// zero — and never revisited that verdict once the pods began to crash. A
	// completed rollout does not un-complete, so the result was a total outage
	// that the gate could not see.
	//
	// brokenRolloutContainer's probe is what stops that happening now.
	// minReadySeconds says the same thing one level up, in a field the kubelet
	// cannot get wrong: a revision counts as available only if it stays Ready
	// this long, so one that dies on startup never counts at all — whatever
	// image or probe a spec puts in front of it.
	dep.Spec.MinReadySeconds = defaultMinReadySeconds
	dep.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			// Zero unavailable, one surge: the new revision gets exactly one pod
			// to fail in and the old revision is never reduced. Any other pair
			// would take capacity away as the rollout stalled, and the fault
			// would announce itself as an availability drop — which is the
			// diagnosis every other kind in this engine already poses.
			MaxUnavailable: ptr(intstr.FromInt32(0)),
			MaxSurge:       ptr(intstr.FromInt32(1)),
		},
	}
	return []runtime.Object{dep}, nil
}

// brokenRolloutContainer is the container the finisher patches in: a different
// image tag running a command that exits non-zero.
//
// Both halves matter. The tag change is what makes this a new revision an
// operator can name; the failing command is what stops it ever becoming ready.
// A tag change alone against an image that runs fine would roll out
// successfully, and the fault would apply cleanly and do nothing.
func brokenRolloutContainer(spec map[string]any) (corev1.Container, error) {
	msg := optString(spec, "message", defaultRolloutMessage)
	if strings.ContainsAny(msg, "\n\r") {
		return corev1.Container{}, fmt.Errorf("spec.message must be a single line")
	}
	return corev1.Container{
		Name:  containerName,
		Image: optString(spec, "broken_image", defaultBrokenRolloutImage),
		// Same environment-variable indirection as every other kind that writes
		// caller-supplied text: interpolating it into the shell string is how a
		// scenario pack that wants a specific log line becomes a command
		// injection.
		Env: []corev1.EnvVar{{Name: exitMessageEnv, Value: msg}},
		Command: []string{"/bin/sh", "-c",
			fmt.Sprintf("echo \"$%s\" >&2; exit 1", exitMessageEnv)},
		// A readiness probe that cannot pass, on a container that is dead most
		// of the time anyway. It is here because of what the crash loop does to
		// the *Deployment*, not to the pod.
		//
		// Without a probe, a pod is Ready the moment its container is running —
		// which a crash-looping container is, briefly, on every restart. The
		// Deployment controller reads each of those flickers as progress and
		// restarts the progress-deadline clock, so ProgressDeadlineExceeded
		// appeared 159s into the first GKE run instead of 60s, and then flipped
		// back to ReplicaSetUpdated on the next restart. A fault that is only
		// intermittently true is worse than one that does not land: the gate
		// catches it and the subject, triaging a minute later, does not.
		//
		// With the probe the new revision is never Ready, so nothing after the
		// initial scale-up counts as progress, the deadline fires once, and the
		// condition stays put for the life of the lease.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/bin/false"}},
			},
			PeriodSeconds:    5,
			FailureThreshold: 1,
		},
	}, nil
}

// certExpiryBundle: a TLS Secret whose certificate is about to expire (or
// already has), and a workload that mounts it.
//
// The workload is not decoration. A Secret alone would be a fault nothing
// consumes — deletable, ignorable, and impossible to gate honestly, since a
// Secret that never mounted anywhere looks exactly like one that did. Mounting
// it means the kubelet refuses to start the container until the Secret exists
// with the keys the volume expects, so a Ready pod is proof the certificate
// reached the cluster in a usable shape.
//
// Nothing verifies the certificate, and nothing here pretends to. TLS expiry is
// a fault of *time*, not of state: the object is well-formed, the pods are
// healthy, and the failure is scheduled rather than occurring. What a subject
// has to do is read notAfter and compare it to the clock, which is exactly the
// step that gets skipped.
func certExpiryBundle(s synthesis) ([]runtime.Object, error) {
	pod, err := healthyPod(s.spec)
	if err != nil {
		return nil, err
	}
	hours, err := optInt(s.spec, "expires_in_hours", catalog.KubeStateDefaultCertHours)
	if err != nil {
		return nil, err
	}
	if hours < catalog.KubeStateMinCertHours || hours > catalog.KubeStateMaxCertHours {
		return nil, fmt.Errorf("spec.expires_in_hours must be between %d and %d, got %d",
			catalog.KubeStateMinCertHours, catalog.KubeStateMaxCertHours, hours)
	}
	cn := optString(s.spec, "common_name", defaultCommonName)
	if errs := validation.IsDNS1123Subdomain(cn); len(errs) > 0 {
		// It goes in a DNS SAN as well as the subject, and a SAN that is not a
		// hostname makes a certificate no TLS stack will accept for a reason
		// that has nothing to do with expiry.
		return nil, fmt.Errorf("spec.common_name %q is not a valid DNS name: %s", cn, strings.Join(errs, "; "))
	}

	crt, err := newExpiringCert(cn, time.Duration(hours)*time.Hour, s.now)
	if err != nil {
		return nil, fmt.Errorf("synthesize certificate: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		// The real TLS type, not Opaque. It is what makes the two keys mean
		// what they are called, what a cert-manager or an Ingress would look
		// for, and what makes `kubectl get secret` show this as a certificate
		// rather than as two opaque blobs.
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       crt.certPEM,
			corev1.TLSPrivateKeyKey: crt.keyPEM,
		},
	}

	pod.Volumes = []corev1.Volume{{
		Name:         certVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: s.name}},
	}}
	pod.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      certVolumeName,
		MountPath: certMountPath,
		ReadOnly:  true,
	}}

	// The Secret first, for the same reason UnboundClaim creates its claim
	// first: a pod referencing a Secret that does not exist yet reports
	// FailedMount, which is a different diagnosis than the one this kind poses
	// and would leave the workload-ready gate waiting on a retry.
	return []runtime.Object{secret, newDeployment(s, pod)}, nil
}

// ptr returns a pointer to v, for the API types that take one.
func ptr[T any](v T) *T { return &v }

// newService builds the Service that fronts a bundle, selecting on the given
// `app` label value.
//
// The selector is a parameter rather than derived from the synthesis because
// whether it matches is the difference between two fault kinds: SelectorDrift
// passes a value the pods do not carry, DependencyStall and BackendCrashLoop
// pass the workload's own name. Everything else about the three Services is
// identical, which is what makes "the selector is wrong" a diagnosis rather
// than a guess at which of several differences mattered.
func newService(s synthesis, port int32, selector string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": s.name},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": selector},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromInt32(port),
			}},
		},
	}
}

// servicePort reads and validates spec.port for the three kinds that publish
// one. Returns int32 rather than int because every consumer is an API field of
// that width, and narrowing here — once, next to the bounds check that makes it
// safe — is what keeps the conversion out of three call sites.
func servicePort(spec map[string]any) (int32, error) {
	port, err := optInt(spec, "port", defaultServicePort)
	if err != nil {
		return 0, err
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("spec.port must be between 1 and 65535, got %d", port)
	}
	return int32(port), nil
}

// newDeployment wraps a pod spec in the Deployment that carries it.
//
// Only `app` goes on here. simian.chaos/managed, the bundle and the fault UID
// are stamped by the driver onto every object in the bundle, because
// ReapExpired has to be able to list what this engine created without help
// from the in-memory registry. None of them reach the pod template: pods are
// what a subject under evaluation inspects, and a pod wearing a label with our
// name on it answers the question the rig is supposed to be asking. None of
// them name the fault either — see bundleLabels for why that was not always
// true.
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
