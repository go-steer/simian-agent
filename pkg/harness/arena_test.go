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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/go-steer/simian-agent/pkg/arena"
)

// serviceAccounted returns a fake clientset that answers the default
// ServiceAccount lookup, so Setup does not spend its wait budget in a test.
func serviceAccounted(objs ...runtime.Object) *fake.Clientset {
	k8s := fake.NewClientset(objs...)
	k8s.PrependReactor("get", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || get.GetName() != "default" {
			return false, nil, nil
		}
		return true, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: get.GetNamespace(),
		}}, nil
	})
	return k8s
}

var networkPolicyGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}

// sweptListKinds registers a list kind for every resource the arena manager
// sweeps before it destroys a namespace. The dynamic fake panics on a List it
// has no list kind for, so a resource added to that sweep shows up here as a
// panic naming it — which is the right way to find out.
func sweptListKinds() map[schema.GroupVersionResource]string {
	kinds := map[schema.GroupVersionResource]string{
		networkPolicyGVR: "NetworkPolicyList",
		{Group: "litmuschaos.io", Version: "v1alpha1", Resource: "chaosengines"}: "ChaosEngineList",
	}
	for _, r := range []string{
		"networkchaos", "podchaos", "iochaos", "stresschaos", "timechaos",
		"kernelchaos", "dnschaos", "httpchaos", "jvmchaos", "blockchaos",
	} {
		kinds[schema.GroupVersionResource{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: r}] = "ChaosList"
	}
	return kinds
}

func newKubeArena(k8s *fake.Clientset) *KubeArena {
	return &KubeArena{
		Manager:     arena.New(k8s, "simian-controller", "simian-system"),
		K8s:         k8s,
		Annotations: map[string]string{"simian.chaos/eval-run": "run-1"},
	}
}

func TestSetupCreatesAnArenaAndTeardownRemovesIt(t *testing.T) {
	k8s := serviceAccounted()
	a := newKubeArena(k8s)

	if err := a.Setup(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ns, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the namespace was not created: %v", err)
	}
	if ns.Annotations[arena.EligibilityAnnotation] != "true" {
		t.Errorf("annotations = %v, want the eligibility marker; nothing can be injected without it", ns.Annotations)
	}
	if ns.Annotations["simian.chaos/eval-run"] != "run-1" {
		t.Errorf("annotations = %v, want the run marker so an abandoned arena can be traced", ns.Annotations)
	}

	if err := a.Teardown(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the namespace this run created is still there: %v", err)
	}
}

// A harness that deletes namespaces it found is one bad scenario file away
// from deleting something that mattered. A namespace that already existed is
// marked as an arena and left standing.
func TestABorrowedNamespaceIsMarkedButNeverDeleted(t *testing.T) {
	k8s := serviceAccounted(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "production"}})
	a := newKubeArena(k8s)

	if err := a.Setup(t.Context(), "production"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := a.Teardown(t.Context(), "production"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "production", metav1.GetOptions{}); err != nil {
		t.Fatalf("a namespace the harness did not create was deleted: %v", err)
	}
}

// The same namespace across two scenarios must stay borrowed. If the second
// Setup overwrote the record with "we created it", the second Teardown would
// delete a namespace the harness found.
func TestBorrowedStaysBorrowedAcrossScenarios(t *testing.T) {
	k8s := serviceAccounted(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared"}})
	a := newKubeArena(k8s)

	for range 2 {
		if err := a.Setup(t.Context(), "shared"); err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if err := a.Teardown(t.Context(), "shared"); err != nil {
			t.Fatalf("Teardown: %v", err)
		}
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "shared", metav1.GetOptions{}); err != nil {
		t.Fatalf("the borrowed namespace was deleted on the second pass: %v", err)
	}
}

// The namespace's real owner deleting it mid-run must not promote the harness
// to owner. It was handed a namespace somebody else manages, and the next
// Setup finding it missing is not a licence to delete it at the end.
func TestANamespaceDeletedByItsOwnerMidRunIsStillNotOurs(t *testing.T) {
	k8s := serviceAccounted(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared"}})
	a := newKubeArena(k8s)

	if err := a.Setup(t.Context(), "shared"); err != nil {
		t.Fatalf("first Setup: %v", err)
	}
	if err := a.Teardown(t.Context(), "shared"); err != nil {
		t.Fatalf("first Teardown: %v", err)
	}
	if err := k8s.CoreV1().Namespaces().Delete(t.Context(), "shared", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := a.Setup(t.Context(), "shared"); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if err := a.Teardown(t.Context(), "shared"); err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "shared", metav1.GetOptions{}); err != nil {
		t.Errorf("a namespace the harness borrowed was deleted after its owner recreated the question: %v", err)
	}
}

// terminatingFor makes the first n namespace lookups answer with a namespace
// in Terminating, and lets every later one fall through to the tracker. It is
// how a namespace deletion that has not finished yet looks from the outside:
// the object is still there, and it answers Get right up until it does not.
func terminatingFor(k8s *fake.Clientset, name string, n int) *int {
	seen := 0
	k8s.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || get.GetName() != name {
			return false, nil, nil
		}
		seen++
		if n >= 0 && seen > n {
			return false, nil, nil
		}
		return true, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
		}, nil
	})
	return &seen
}

// Running a pack twice in a row is the most ordinary thing anyone does with
// this harness, and namespace deletion is asynchronous. The second run waits
// for the first run's teardown rather than failing on it.
func TestANamespaceStillTerminatingFromTheLastRunIsWaitedForAndThenOurs(t *testing.T) {
	k8s := serviceAccounted()
	seen := terminatingFor(k8s, "ns-a", 1)
	a := newKubeArena(k8s)

	if err := a.Setup(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Setup did not wait out a terminating namespace: %v", err)
	}
	if *seen < 2 {
		t.Errorf("namespace looked up %d times, want at least 2: it cannot have waited", *seen)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{}); err != nil {
		t.Fatalf("the arena was not created after the wait: %v", err)
	}

	// And it is ours: what the previous run left behind was a name, not an
	// object, so the namespace standing here now is one this run made.
	if err := a.Teardown(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a namespace this run created was left standing: %v", err)
	}
}

// A namespace stuck on a finalizer is never coming back, and a run should say
// so rather than sit there. The error has to name the wait, because the API
// server's own refusal talks about content in a terminating namespace and
// sends whoever reads it looking for the wrong thing.
func TestAWaitForTerminationThatRunsOutIsAnInjectError(t *testing.T) {
	k8s := serviceAccounted()
	terminatingFor(k8s, "stuck", -1)
	a := newKubeArena(k8s)
	a.TerminatingWait = time.Millisecond

	err := a.Setup(t.Context(), "stuck")
	if err == nil {
		t.Fatal("Setup succeeded against a namespace that never finished terminating")
	}
	for _, want := range []string{"stuck", "Terminating", "finalizer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCancellingTheRunStopsTheTerminationWait(t *testing.T) {
	k8s := serviceAccounted()
	terminatingFor(k8s, "stuck", -1)
	a := newKubeArena(k8s)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := a.Setup(ctx, "stuck")
	if err == nil {
		t.Fatal("Setup kept waiting after the run was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// A live namespace is not a terminating one. Were this confused, every
// borrowed namespace in a pack would spend the whole wait budget before
// failing, and a run against an existing SUT would never start.
func TestAnActiveNamespaceIsNotWaitedFor(t *testing.T) {
	k8s := serviceAccounted(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "production"},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})
	a := newKubeArena(k8s)
	a.TerminatingWait = time.Millisecond

	if err := a.Setup(t.Context(), "production"); err != nil {
		t.Fatalf("Setup treated a live namespace as one on its way out: %v", err)
	}
}

func TestKeepArenasLeavesEvenAnArenaItCreated(t *testing.T) {
	k8s := serviceAccounted()
	a := newKubeArena(k8s)
	a.KeepArenas = true

	if err := a.Setup(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := a.Teardown(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{}); err != nil {
		t.Fatalf("--keep-arenas did not keep the arena: %v", err)
	}
}

// A cluster that will not answer the existence question is not a cluster where
// creating the namespace and hoping is safe: "not found" and "cannot tell" are
// different answers, and only one of them means the namespace is ours.
func TestAnUnansweredLookupFailsSetup(t *testing.T) {
	k8s := serviceAccounted()
	k8s.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("api server is having a day")
	})
	a := newKubeArena(k8s)

	if err := a.Setup(t.Context(), "ns-a"); err == nil {
		t.Fatal("Setup succeeded against an API server that would not answer")
	}
}

// Every pod in a fresh namespace needs the default ServiceAccount, and for a
// moment after the namespace appears it does not exist. A workload applied in
// that window is rejected, and a fault that arrived early looks exactly like a
// fault that failed to inject.
func TestSetupWaitsForTheDefaultServiceAccount(t *testing.T) {
	k8s := fake.NewClientset()
	var gets int
	k8s.PrependReactor("get", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets < 3 {
			return true, nil, apierrors.NewNotFound(corev1.Resource("serviceaccounts"), "default")
		}
		return true, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns-a"}}, nil
	})

	a := newKubeArena(k8s)
	if err := a.Setup(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if gets < 3 {
		t.Errorf("the ServiceAccount was looked up %d times; Setup did not wait", gets)
	}
}

// Waiting out the full budget on an error that will never clear turns a
// misconfigured RBAC into thirty seconds of nothing per scenario, and then
// reports it as a timeout instead of as the permission problem it is.
func TestAnErrorLookingUpTheServiceAccountIsNotAWait(t *testing.T) {
	k8s := fake.NewClientset()
	k8s.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("serviceaccounts"), "default", errBoom)
	})

	a := newKubeArena(k8s)
	err := a.Setup(t.Context(), "ns-a")
	if err == nil {
		t.Fatal("Setup succeeded despite being unable to read the namespace's ServiceAccounts")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %v, want the reason the lookup failed", err)
	}
	if strings.Contains(err.Error(), serviceAccountWait.String()) {
		t.Errorf("error = %v, want it reported without spending the whole wait budget on it", err)
	}
}

// "Not found" and "cannot tell" are different answers, and only the first one
// means the namespace is the harness's to destroy later. A lookup that fails
// for any other reason is refused rather than guessed at.
func TestALookupThatFailsForAnotherReasonIsNotTakenAsAbsence(t *testing.T) {
	k8s := serviceAccounted()
	var gets int
	k8s.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewServiceUnavailable("api server is having a day")
		}
		return false, nil, nil
	})

	a := newKubeArena(k8s)
	if err := a.Setup(t.Context(), "ns-a"); err == nil {
		t.Fatal("Setup treated an unanswerable lookup as a namespace that does not exist")
	}
}

// Destroy is asked without force on purpose: the runner has already cleared
// every fault it applied, so a refusal means something else put chaos in this
// namespace, and deleting the namespace out from under it is how a rig takes
// down more than it injected.
func TestTeardownDoesNotForceDestroyOverLiveChaos(t *testing.T) {
	k8s := serviceAccounted()
	a := newKubeArena(k8s)

	np := &unstructured.Unstructured{}
	np.SetAPIVersion("networking.k8s.io/v1")
	np.SetKind("NetworkPolicy")
	np.SetNamespace("ns-a")
	np.SetName("simian-np-01jt")
	np.SetLabels(map[string]string{arena.SimianManagedFaultLabel: "true"})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), sweptListKinds())
	if _, err := dyn.Resource(networkPolicyGVR).Namespace("ns-a").Create(t.Context(), np, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the leftover partition: %v", err)
	}
	a.Manager.Dyn = dyn

	if err := a.Setup(t.Context(), "ns-a"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	err := a.Teardown(t.Context(), "ns-a")
	if err == nil {
		t.Fatal("Teardown destroyed an arena with simian-managed chaos still live in it")
	}
	if !strings.Contains(err.Error(), "still active") {
		t.Errorf("error = %v, want it to say what is still in the namespace", err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(t.Context(), "ns-a", metav1.GetOptions{}); err != nil {
		t.Errorf("the namespace was deleted despite the refusal: %v", err)
	}
}
