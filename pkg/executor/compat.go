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
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
// containers it targets. Only what can be read is judged: a target without
// labels, or whose pods cannot be listed, is left to the injection check.
func (e *Executor) checkTargetCompat(ctx context.Context, m simian.FaultManifest) error {
	if e.pods == nil || !WritesRootFS(m.Engine, m.ResourceKind) {
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
