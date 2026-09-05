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

	corev1 "k8s.io/api/core/v1"
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

// DefaultTerminatingWait bounds the wait for a namespace left over from a
// previous run to finish deleting.
//
// Namespace deletion is asynchronous, and a namespace in Terminating rejects
// every create with a message about content, not about deletion. Running a
// pack twice in a row is the most ordinary thing anyone does with this
// harness, so the second run waits for the first run's teardown instead of
// failing on it.
//
// Two minutes because the wait is for the API server's own object GC, which
// takes seconds on an empty arena; anything longer than this is a finalizer
// that is not coming back, and a run should say so rather than sit there.
const DefaultTerminatingWait = 2 * time.Minute

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

	// TerminatingWait bounds the wait for a leftover namespace to finish
	// deleting. Zero means DefaultTerminatingWait — the zero value is the
	// patient one, because a harness assembled without thinking about this
	// should wait rather than flake. Set it small to make the wait effectively
	// no wait at all; there is no separate way to switch it off.
	TerminatingWait time.Duration

	mu      sync.Mutex
	created map[string]bool
}

// Setup makes namespace an arena, creating it if it does not exist.
func (a *KubeArena) Setup(ctx context.Context, namespace string) error {
	mine, err := a.claim(ctx, namespace)
	if err != nil {
		return err
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

// claim reports whether the namespace will be ours to destroy later, waiting
// out one that is still terminating from an earlier run.
//
// "Not found" and "cannot tell" are different answers, and only the first one
// means the namespace is ours. A namespace that finishes terminating while we
// watch becomes ours too: what is left afterwards is a name, not an object,
// and the next create makes it from nothing exactly as it would have.
func (a *KubeArena) claim(ctx context.Context, namespace string) (bool, error) {
	wait := a.TerminatingWait
	if wait == 0 {
		wait = DefaultTerminatingWait
	}
	deadline := time.Now().Add(wait)
	announced := false

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		ns, err := a.K8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("looking up namespace: %w", err)
		case ns.Status.Phase != corev1.NamespaceTerminating:
			return false, nil
		}

		if !time.Now().Before(deadline) {
			// Named as a wait that ran out rather than as a create that was
			// refused: the API server's own message for the latter talks about
			// content in a terminating namespace, which sends whoever reads it
			// looking for the wrong thing.
			return false, fmt.Errorf(
				"namespace %s is still Terminating after %s: a previous run may still be tearing it down, or something in it has a finalizer that is not completing",
				namespace, wait)
		}
		if !announced {
			// Once, not once per poll: a two-minute wait at this interval is
			// 240 identical lines, and the run's log is something a person
			// reads.
			announced = true
			a.logger().Info("harness: waiting for a namespace to finish terminating",
				slog.String("namespace", namespace),
				slog.Duration("timeout", wait))
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("waiting for namespace %s to finish terminating: %w", namespace, ctx.Err())
		case <-ticker.C:
		}
	}
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
