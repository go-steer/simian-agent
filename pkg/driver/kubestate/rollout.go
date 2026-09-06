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
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// finisher is a second step for a kind whose failure state cannot be created,
// only arrived at.
//
// Every other kind in this engine is born broken, which is what makes
// synthesize mode deterministic: one Create and the fault exists. A stuck
// rollout is not a state an object can be created in — it is a relationship
// between a revision that works and a revision that does not, and there is no
// way to express that in a single Deployment manifest. The finisher runs inside
// Apply, after the bundle is created and before Apply returns, so the fault is
// either fully landed or fully rolled back and the executor never sees a
// half-applied one.
type finisher func(ctx context.Context, c kubernetes.Interface, s synthesis) error

var finishers = map[string]finisher{
	KindRolloutStuck: wedgeRollout,
}

// wedgeRollout waits for the first revision to be fully available, then rolls
// out a second one that can never become ready.
//
// The wait is the whole difficulty. Patching immediately would produce a
// Deployment that never had a working revision at all: the gate's second
// assertion — every replica of the previous revision still available — would be
// false, and more importantly the fault would be a lie. "A deploy broke and
// nobody noticed because the old pods kept serving" requires old pods that were
// serving.
func wedgeRollout(ctx context.Context, c kubernetes.Interface, s synthesis) error {
	if err := waitRolloutComplete(ctx, c, s); err != nil {
		return err
	}
	broken, err := brokenRolloutContainer(s.spec)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					// A strategic merge patch, so the containers array merges on
					// the container name rather than replacing the array
					// wholesale. Replacing it would drop anything the healthy
					// pod spec set that this patch does not mention.
					"containers": []corev1.Container{broken},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("build rollout patch: %w", err)
	}
	if _, err := c.AppsV1().Deployments(s.namespace).Patch(
		ctx, s.name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch deployment %s/%s to the broken revision: %w", s.namespace, s.name, err)
	}
	return nil
}

// waitRolloutComplete blocks until the Deployment reports every replica
// available for the generation the controller has actually observed.
//
// Both halves of that are needed. availableReplicas alone can be stale — it is
// left over from the previous observation until the controller catches up — so
// a check that ignored observedGeneration could pass against a status written
// before the Deployment existed in its current shape.
func waitRolloutComplete(ctx context.Context, c kubernetes.Interface, s synthesis) error {
	timeout := s.settle
	if timeout <= 0 {
		timeout = rolloutSettleTimeout
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		dep, err := c.AppsV1().Deployments(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s/%s: %w", s.namespace, s.name, err)
		}
		if rolloutComplete(dep) {
			return nil
		}
		last = fmt.Sprintf("observedGeneration %d of %d, %d/%d replicas available",
			dep.Status.ObservedGeneration, dep.Generation, dep.Status.AvailableReplicas, s.replicas)

		if !time.Now().Before(deadline) {
			// A timeout here means the healthy revision never came up, which is
			// a broken arena rather than a landed fault. Apply rolls the bundle
			// back and reports it, instead of wedging a rollout that had nothing
			// to wedge and letting the gate call it a success.
			return fmt.Errorf(
				"deployment %s/%s did not become fully available within %s (%s): the healthy revision has to be serving before it can be wedged",
				s.namespace, s.name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for deployment %s/%s to become available: %w", s.namespace, s.name, ctx.Err())
		case <-time.After(rolloutPollInterval):
		}
	}
}

func rolloutComplete(dep *appsv1.Deployment) bool {
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return dep.Status.AvailableReplicas >= want && dep.Status.UpdatedReplicas >= want
}
