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

package harness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/arena"
)

// serviceAccountWait bounds the wait for a fresh namespace's default
// ServiceAccount. Every pod in the namespace needs it, and for a second or so
// after the namespace appears it does not exist — a workload applied into that
// window is rejected, and a fault that looks like it failed to inject when it
// only arrived early is the most expensive kind of flake here.
const serviceAccountWait = 30 * time.Second

// KubeArena is the cluster-backed Arena: it marks a namespace as a place chaos
// is permitted, and puts it back.
//
// It destroys only what it created. A scenario that names a namespace which
// already existed gets that namespace marked as an arena and left standing
// afterwards, because a harness that deletes namespaces it found is one bad
// scenario file away from deleting something that mattered. The marking is
// left behind too, and said so in the log — an annotation an operator can see
// and remove is a smaller surprise than a namespace that is gone.
type KubeArena struct {
	Manager *arena.Manager
	K8s     kubernetes.Interface
	Logger  *slog.Logger

	// Annotations are merged onto every arena this harness creates. The
	// per-run marker goes here, so an abandoned run is identifiable.
	Annotations map[string]string

	// KeepArenas leaves every namespace standing after the run, for looking at
	// a scenario that went wrong. Faults are still cleared: a namespace kept
	// for inspection is useful, a partition kept by accident is an outage.
	KeepArenas bool

	mu      sync.Mutex
	created map[string]bool
}

// Setup makes namespace an arena, creating it if it does not exist.
func (a *KubeArena) Setup(ctx context.Context, namespace string) error {
	_, err := a.K8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	mine := apierrors.IsNotFound(err)
	// "Not found" and "cannot tell" are different answers, and only the first
	// one means the namespace is ours to destroy later.
	if err != nil && !mine {
		return fmt.Errorf("looking up namespace: %w", err)
	}
	a.record(namespace, mine)

	if err := a.Manager.Create(ctx, arena.Spec{
		Namespace:        namespace,
		ExtraAnnotations: a.Annotations,
	}); err != nil {
		return err
	}
	if !mine {
		return nil
	}
	return a.waitForDefaultServiceAccount(ctx, namespace)
}

// Teardown destroys an arena this harness created, and leaves alone one it
// merely borrowed.
func (a *KubeArena) Teardown(ctx context.Context, namespace string) error {
	if a.KeepArenas {
		a.logger().Info("harness: keeping arena", slog.String("namespace", namespace))
		return nil
	}
	if !a.isOurs(namespace) {
		a.logger().Info("harness: leaving borrowed namespace in place",
			slog.String("namespace", namespace),
			slog.String("note", "it existed before this run; the simian.chaos/eligible annotation is still on it"))
		return nil
	}
	// force=false on purpose: Destroy refusing because chaos is still leased
	// is a signal worth surfacing, not one worth overriding. The runner has
	// already cleared every fault it applied, so a refusal here means
	// something else put chaos in this namespace.
	return a.Manager.Destroy(ctx, namespace, false)
}

// waitForDefaultServiceAccount blocks until the namespace's default
// ServiceAccount exists.
func (a *KubeArena) waitForDefaultServiceAccount(ctx context.Context, namespace string) error {
	ctx, cancel := context.WithTimeout(ctx, serviceAccountWait)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := a.K8s.CoreV1().ServiceAccounts(namespace).Get(ctx, "default", metav1.GetOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("waiting for default ServiceAccount in %s: %w", namespace, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s has no default ServiceAccount after %s: %w", namespace, serviceAccountWait, ctx.Err())
		case <-ticker.C:
		}
	}
}

// record notes who a namespace belongs to. Borrowed wins permanently: the
// second scenario to run in a namespace finds it existing and would otherwise
// record it as created, and the namespace the harness borrowed on the first
// scenario would be deleted on the second.
func (a *KubeArena) record(ns string, mine bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.created == nil {
		a.created = map[string]bool{}
	}
	if _, seen := a.created[ns]; seen {
		return
	}
	a.created[ns] = mine
}

// isOurs reports whether this harness created the namespace. A namespace it
// never saw is not ours, which is the safe answer: Teardown destroys only what
// it is sure it made.
func (a *KubeArena) isOurs(ns string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.created[ns]
}

func (a *KubeArena) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.New(slog.DiscardHandler)
}
