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

package executor

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// TargetPods lists the pods a target's labels select.
type TargetPods interface {
	Pods(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error)
}

// WithTargetPods lets the executor refuse a fault its engine cannot inject
// into the pods it targets, before the driver runs.
//
// Chaos Mesh IOChaos and DNSChaos write into the target container's root
// filesystem — IOChaos moves the volume aside next to its mount point,
// DNSChaos backs up /etc/resolv.conf — and both fail against a container
// with readOnlyRootFilesystem. On the 2026-09-30 trial every IOChaos against
// Online Boutique did, and a failed IOChaos then sits behind its finalizer
// (#163).
func WithTargetPods(tp TargetPods) Option {
	return func(e *Executor) { e.pods = tp }
}

// KubernetesTargetPods lists pods with the Kubernetes API.
type KubernetesTargetPods struct {
	Client kubernetes.Interface
}

// Pods implements TargetPods.
func (k KubernetesTargetPods) Pods(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	list, err := k.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(selector).String(),
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// rootFSWriters are the Chaos Mesh kinds that write into the target
// container's root filesystem to inject.
var rootFSWriters = map[string]bool{"IOChaos": true, "DNSChaos": true}

// WritesRootFS reports whether the engine writes into the target container's
// root filesystem to inject this kind, so cannot inject it into one mounted
// read-only.
func WritesRootFS(engine simian.Engine, kind string) bool {
	return engine == simian.EngineChaosMesh && rootFSWriters[kind]
}

// checkTargetCompat refuses a fault whose engine cannot inject it into the
// containers it targets, or cannot take it back out. Only what can be read is
// judged: a target without labels, or whose pods cannot be listed, is left to
// the injection check.
func (e *Executor) checkTargetCompat(ctx context.Context, m simian.FaultManifest) error {
	if e.pods == nil || m.Engine != simian.EngineChaosMesh {
		return nil
	}
	if m.ResourceKind == "HTTPChaos" {
		return e.checkHTTPChaosProbes(ctx, m)
	}
	if !WritesRootFS(m.Engine, m.ResourceKind) {
		return nil
	}
	containers := specStrings(m.Spec, "containerNames")
	volumePath, _ := m.Spec["volumePath"].(string)
	for _, t := range m.Targets {
		if t.Namespace == "" || len(t.Labels) == 0 {
			continue
		}
		pods, err := e.pods.Pods(ctx, t.Namespace, t.Labels)
		if err != nil {
			continue
		}
		for _, p := range pods {
			for _, c := range p.Spec.Containers {
				if len(containers) > 0 && !slices.Contains(containers, c.Name) {
					continue
				}
				where := fmt.Sprintf("%s/%s container %q", p.Namespace, p.Name, c.Name)
				if c.SecurityContext != nil && c.SecurityContext.ReadOnlyRootFilesystem != nil && *c.SecurityContext.ReadOnlyRootFilesystem {
					return simian.NewExecutorError(simian.StagePrecheck, simian.ReasonTargetIncompatible,
						fmt.Sprintf("%s has a read-only root filesystem, which Chaos Mesh %s needs to write to", where, m.ResourceKind), nil)
				}
				if m.ResourceKind == "IOChaos" && volumePath != "" && !mountsAt(c, volumePath) {
					return simian.NewExecutorError(simian.StagePrecheck, simian.ReasonTargetIncompatible,
						fmt.Sprintf("%s has no volume mounted at volumePath %q; IOChaos needs a mount point (mounts: %v)", where, volumePath, mountPaths(c)), nil)
				}
			}
		}
	}
	return nil
}

func mountsAt(c corev1.Container, p string) bool {
	return slices.Contains(mountPaths(c), path.Clean(p))
}

func mountPaths(c corev1.Container) []string {
	out := make([]string, 0, len(c.VolumeMounts))
	for _, vm := range c.VolumeMounts {
		out = append(out, path.Clean(vm.MountPath))
	}
	return out
}

func specStrings(spec map[string]any, key string) []string {
	raw, _ := spec[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// checkHTTPChaosProbes refuses an HTTPChaos aimed at a gRPC port, or one that
// would hit a target container's liveness or startup probe.
//
// HTTPChaos works through a proxy Chaos Mesh runs in the pod's network
// namespace, with redirect rules in front of it. A fault on the probe's path
// fails the probe, the kubelet restarts the container, and the proxy dies with
// it while the redirect rules stay: the port is a black hole that outlives the
// fault, and every restart fails the probe again. On the 2026-10-03 soak a
// three-minute abort on bank's frontend left it down until the pod was
// deleted by hand. A readiness probe is left alone — failing it is the
// symptom such a fault is for, and it restarts nothing.
func (e *Executor) checkHTTPChaosProbes(ctx context.Context, m simian.FaultManifest) error {
	port, ok := specInt(m.Spec, "port")
	if !ok {
		return nil
	}
	method, _ := m.Spec["method"].(string)
	getsProbes := method == "" || strings.EqualFold(method, "GET") // kubelet HTTP probes are GETs
	pathGlob, _ := m.Spec["path"].(string)
	for _, t := range m.Targets {
		if t.Namespace == "" || len(t.Labels) == 0 {
			continue
		}
		pods, err := e.pods.Pods(ctx, t.Namespace, t.Labels)
		if err != nil {
			continue
		}
		for _, p := range pods {
			for _, c := range p.Spec.Containers {
				if GRPCPort(c, port) {
					return simian.NewExecutorError(simian.StagePrecheck, simian.ReasonTargetIncompatible,
						fmt.Sprintf("%s/%s container %q serves gRPC on port %d; Chaos Mesh's HTTPChaos proxy handles HTTP/1 only, so it fails the gRPC traffic and probes there, and a restart that follows leaves the port unreachable after the fault ends — use a NetworkChaos for this port",
							p.Namespace, p.Name, c.Name, port), nil)
				}
				for _, rp := range []struct {
					kind  string
					probe *corev1.Probe
				}{{"liveness", c.LivenessProbe}, {"startup", c.StartupProbe}} {
					kind, probe := rp.kind, rp.probe
					if !getsProbes || probe == nil || probe.HTTPGet == nil || probePort(c, probe.HTTPGet.Port) != port || !ChaosPathMatches(pathGlob, probe.HTTPGet.Path) {
						continue
					}
					return simian.NewExecutorError(simian.StagePrecheck, simian.ReasonTargetIncompatible,
						fmt.Sprintf("%s/%s container %q has a %s probe on GET :%d%s, which this HTTPChaos would fail; the restart that follows kills Chaos Mesh's proxy and leaves the port unreachable after the fault ends — target a path the probe does not use",
							p.Namespace, p.Name, c.Name, kind, port, probe.HTTPGet.Path), nil)
				}
			}
		}
	}
	return nil
}

// probePort resolves a probe's port, named or numbered, against the
// container's declared ports. 0 when a name does not resolve.
func probePort(c corev1.Container, port intstr.IntOrString) int {
	if port.Type == intstr.Int {
		return port.IntValue()
	}
	for _, cp := range c.Ports {
		if cp.Name == port.StrVal {
			return int(cp.ContainerPort)
		}
	}
	return 0
}

// ChaosPathMatches reports whether an HTTPChaos path selector matches a
// request path. Exported for the planner, which rejects the same steps. An empty selector matches every path. "*" is matched across
// "/" as well as within a segment: wider than Chaos Mesh may be, and on the
// side of refusing a fault that would not have hit the probe rather than
// running one that would.
func ChaosPathMatches(glob, p string) bool {
	if glob == "" {
		return true
	}
	parts := strings.Split(glob, "*")
	expr := "^"
	for i, part := range parts {
		if i > 0 {
			expr += ".*"
		}
		expr += regexp.QuoteMeta(part)
	}
	re, err := regexp.Compile(expr + "$")
	return err == nil && re.MatchString(p)
}

// specInt reads a number from a decoded spec, which JSON gives as float64.
func specInt(spec map[string]any, key string) (int, bool) {
	switch v := spec[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// GRPCPort reports whether the container serves gRPC on port: a gRPC probe
// of any kind is aimed at it, or the port is named grpc. Online Boutique
// declares its ports unnamed and probes them with grpc, so the probe is the
// signal that matters. On the 2026-10-04 soak an HTTPChaos on paymentservice's
// gRPC port left it unreachable for twelve hours, past a check that looked
// only at HTTP probes.
func GRPCPort(c corev1.Container, port int) bool {
	for _, probe := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		if probe != nil && probe.GRPC != nil && int(probe.GRPC.Port) == port {
			return true
		}
	}
	for _, cp := range c.Ports {
		if int(cp.ContainerPort) == port && strings.HasPrefix(strings.ToLower(cp.Name), "grpc") {
			return true
		}
	}
	return false
}
