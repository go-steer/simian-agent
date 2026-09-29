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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// WorkloadSelectors finds the pod labels behind a named workload.
type WorkloadSelectors interface {
	// PodLabels returns the matchLabels of the workload's pod selector. kind
	// may be empty, meaning any workload kind with that name.
	PodLabels(ctx context.Context, namespace, kind, name string) (map[string]string, error)
}

// WithWorkloadSelectors lets the executor fill in the labels of a target that
// names a workload but does not say which pods it has.
//
// Every default probe finds its pods by targets[0].labels. A target carrying
// only a name — what `simian chaos --workload` builds, and what a planner
// writes often enough — gave them nothing to select, so the fault failed its
// own precheck before it was applied (#144).
func WithWorkloadSelectors(ws WorkloadSelectors) Option {
	return func(e *Executor) { e.workloads = ws }
}

// resolveTargetLabels fills in Labels on each target that has a Name and no
// Labels. It returns the targets it resolved and, separately, the ones it
// could not, for the audit record. A target it cannot resolve is left alone:
// the gate then fails or passes on what the manifest said, as it did before.
func (e *Executor) resolveTargetLabels(ctx context.Context, m *simian.FaultManifest) (resolved, failed []string) {
	if e.workloads == nil {
		return nil, nil
	}
	for i := range m.Targets {
		t := &m.Targets[i]
		if t.Name == "" || len(t.Labels) > 0 {
			continue
		}
		ref := t.Namespace + "/" + t.Name
		labels, err := e.workloads.PodLabels(ctx, t.Namespace, t.Kind, t.Name)
		if err != nil {
			failed = append(failed, ref+": "+err.Error())
			continue
		}
		t.Labels = labels
		resolved = append(resolved, ref)
	}
	return resolved, failed
}

// KubernetesWorkloadSelectors reads pod selectors from Deployments,
// StatefulSets and DaemonSets.
type KubernetesWorkloadSelectors struct {
	Client kubernetes.Interface
}

// PodLabels implements WorkloadSelectors.
func (k KubernetesWorkloadSelectors) PodLabels(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	getters := []struct {
		kind string
		get  func() (*metav1.LabelSelector, error)
	}{
		{"Deployment", func() (*metav1.LabelSelector, error) {
			d, err := k.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return d.Spec.Selector, nil
		}},
		{"StatefulSet", func() (*metav1.LabelSelector, error) {
			s, err := k.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return s.Spec.Selector, nil
		}},
		{"DaemonSet", func() (*metav1.LabelSelector, error) {
			d, err := k.Client.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return d.Spec.Selector, nil
		}},
	}
	tried := false
	for _, g := range getters {
		if kind != "" && kind != g.kind {
			continue
		}
		tried = true
		sel, err := g.get()
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", g.kind, err)
		}
		// matchExpressions cannot be written as a label map; a selector that
		// relies on them is left for the manifest's author to spell out.
		if sel == nil || len(sel.MatchLabels) == 0 || len(sel.MatchExpressions) > 0 {
			return nil, fmt.Errorf("%s %s selects pods by expression, not labels alone", g.kind, name)
		}
		out := make(map[string]string, len(sel.MatchLabels))
		for key, v := range sel.MatchLabels {
			out[key] = v
		}
		return out, nil
	}
	if !tried {
		return nil, fmt.Errorf("kind %q has no pod selector Simian knows how to read", kind)
	}
	return nil, fmt.Errorf("no Deployment, StatefulSet or DaemonSet named %q", name)
}
