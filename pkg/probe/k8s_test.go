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

package probe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

var (
	podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	epsGVR  = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
)

// testMapper knows the handful of resources the probe tests name.
func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}})
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"}, meta.RESTScopeNamespace)
	return m
}

func newFakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		podsGVR: "PodList",
		epsGVR:  "EndpointSliceList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
}

// pod builds an unstructured pod whose single container reports waitingReason
// (empty means the container is running and has no waiting state at all,
// which is the shape a probe sees before the fault lands).
func pod(ns, name, waitingReason string) *unstructured.Unstructured {
	status := map[string]any{"phase": "Running"}
	cs := map[string]any{"name": "app"}
	if waitingReason != "" {
		cs["state"] = map[string]any{"waiting": map[string]any{"reason": waitingReason}}
	} else {
		cs["state"] = map[string]any{"running": map[string]any{}}
	}
	status["containerStatuses"] = []any{cs}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    map[string]any{"app": "payments"},
		},
		"status": status,
	}}
}

// crashLoopSpec is the settle condition for a CrashLoopBackOff fault, written
// exactly as the eval fixtures write it for kubectl.
func crashLoopSpec() map[string]any {
	return map[string]any{
		"resource":        "pods",
		"jsonpath":        "{.items[*].status.containerStatuses[*].state.waiting.reason}",
		"expect_contains": "CrashLoopBackOff",
		"timeout":         "1s",
		"interval":        "10ms",
	}
}

func k8sProbe(spec map[string]any) simian.ProbeSpec {
	return simian.ProbeSpec{Name: "settle", Type: simian.ProbeTypeK8s, Mode: simian.ProbeModeSettle, Spec: spec}
}

func TestK8sProbePassesWhenTheClusterAlreadySaysWhatWeExpect(t *testing.T) {
	dyn := newFakeDyn(pod("boutique", "payments-1", "CrashLoopBackOff"))
	p := NewK8sProber(dyn, testMapper())

	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if res.Err != nil {
		t.Fatalf("Err: %v", res.Err)
	}
	if !res.Passed {
		t.Fatalf("probe did not pass; observed %q", res.Observed)
	}
	if res.Observed != "CrashLoopBackOff" {
		t.Errorf("Observed=%q, want %q", res.Observed, "CrashLoopBackOff")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts=%d, want 1", res.Attempts)
	}
}

func TestK8sProbeKeepsPollingUntilTheFaultActuallyManifests(t *testing.T) {
	// A CrashLoopBackOff needs two restarts and a backoff to exist at all.
	// Returning healthy on the first read is the normal case, not a failure.
	dyn := newFakeDyn()
	var mu sync.Mutex
	reads := 0
	dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		reason := ""
		if n >= 3 {
			reason = "CrashLoopBackOff"
		}
		list := &unstructured.UnstructuredList{Object: map[string]any{
			"apiVersion": "v1", "kind": "PodList",
		}}
		list.Items = []unstructured.Unstructured{*pod("boutique", "payments-1", reason)}
		return true, list, nil
	})
	p := NewK8sProber(dyn, testMapper())

	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if !res.Passed {
		t.Fatalf("probe never passed; observed %q, attempts %d", res.Observed, res.Attempts)
	}
	if res.Attempts < 3 {
		t.Errorf("Attempts=%d, want at least 3 — the probe should have waited", res.Attempts)
	}
}

func TestK8sProbeTimesOutReportingWhatItLastSaw(t *testing.T) {
	// The pod is healthy and stays that way: the fault did not land.
	dyn := newFakeDyn(pod("boutique", "payments-1", "ImagePullBackOff"))
	p := NewK8sProber(dyn, testMapper())

	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if res.Passed {
		t.Fatal("probe passed against a pod that never crash-looped")
	}
	// Ran fine, just was not satisfied — that is not an error.
	if res.Err != nil {
		t.Errorf("Err=%v, want nil for a probe that ran cleanly and was unsatisfied", res.Err)
	}
	if res.Observed != "ImagePullBackOff" {
		t.Errorf("Observed=%q, want the value actually read", res.Observed)
	}
	if !strings.Contains(res.Describe(), "ImagePullBackOff") {
		t.Errorf("Describe() hides what was seen: %s", res.Describe())
	}
	if !strings.Contains(res.Describe(), "CrashLoopBackOff") {
		t.Errorf("Describe() hides what was wanted: %s", res.Describe())
	}
}

func TestK8sProbeHandlesAFaultWhoseSignatureIsAnAbsence(t *testing.T) {
	// A Service with no ready endpoints has no string to match on. This is
	// why expect_empty exists.
	spec := map[string]any{
		"resource":     "endpointslices",
		"jsonpath":     "{.items[*].endpoints[*].addresses[*]}",
		"expect_empty": true,
		"timeout":      "1s",
		"interval":     "10ms",
	}
	empty := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata":  map[string]any{"name": "frontend-abc", "namespace": "boutique"},
		"endpoints": []any{},
	}}
	p := NewK8sProber(newFakeDyn(empty), testMapper())
	res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res.Err != nil {
		t.Fatalf("Err: %v", res.Err)
	}
	if !res.Passed {
		t.Fatalf("expect_empty did not pass on an empty endpoint list; observed %q", res.Observed)
	}

	// ... and does not pass once an address shows up.
	populated := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{"name": "frontend-abc", "namespace": "boutique"},
		"endpoints": []any{map[string]any{
			"addresses": []any{"10.1.2.3"},
		}},
	}}
	p2 := NewK8sProber(newFakeDyn(populated), testMapper())
	res2 := p2.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res2.Passed {
		t.Fatalf("expect_empty passed with an address present: observed %q", res2.Observed)
	}
}

func TestK8sProbeRejectsSpecsThatWouldPassUnconditionally(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"resource": "pods", "jsonpath": "{.items[*].status.phase}"}
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantSub string
	}{
		{
			// strings.Contains(anything, "") is always true. A probe like this
			// reads as a check and is not one.
			name:    "no condition at all",
			mutate:  func(map[string]any) {},
			wantSub: "passes unconditionally",
		},
		{
			name:    "explicitly empty expect_contains",
			mutate:  func(s map[string]any) { s["expect_contains"] = "" },
			wantSub: "passes unconditionally",
		},
		{
			name: "both conditions at once",
			mutate: func(s map[string]any) {
				s["expect_contains"] = "Running"
				s["expect_empty"] = true
			},
			wantSub: "mutually exclusive",
		},
		{
			name:    "no resource",
			mutate:  func(s map[string]any) { delete(s, "resource"); s["expect_contains"] = "x" },
			wantSub: `"resource" is required`,
		},
		{
			name:    "no jsonpath",
			mutate:  func(s map[string]any) { delete(s, "jsonpath"); s["expect_contains"] = "x" },
			wantSub: `"jsonpath" is required`,
		},
		{
			name: "name and label_selector together",
			mutate: func(s map[string]any) {
				s["expect_contains"] = "x"
				s["name"] = "payments-1"
				s["label_selector"] = "app=payments"
			},
			wantSub: "mutually exclusive",
		},
		{
			// "timeout": 30 is ambiguous between seconds and nanoseconds, and
			// guessing wrong turns the gate into a no-op or a hang.
			name: "numeric timeout",
			mutate: func(s map[string]any) {
				s["expect_contains"] = "x"
				s["timeout"] = 30
			},
			wantSub: "duration string",
		},
		{
			name: "unparseable timeout",
			mutate: func(s map[string]any) {
				s["expect_contains"] = "x"
				s["timeout"] = "thirty seconds"
			},
			wantSub: `"timeout"`,
		},
		{
			name: "zero timeout",
			mutate: func(s map[string]any) {
				s["expect_contains"] = "x"
				s["timeout"] = "0s"
			},
			wantSub: "must be positive",
		},
	}
	p := NewK8sProber(newFakeDyn(), testMapper())
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(s)
			res := p.Run(context.Background(), k8sProbe(s), Target{Namespace: "boutique"})
			if res.Err == nil {
				t.Fatalf("spec accepted, want rejection (passed=%v)", res.Passed)
			}
			if res.Passed {
				t.Error("a rejected spec must not report a pass")
			}
			if !strings.Contains(res.Err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", res.Err, tc.wantSub)
			}
		})
	}
}

func TestK8sProbeReadsOneNamedObjectWhenGivenAName(t *testing.T) {
	dyn := newFakeDyn(
		pod("boutique", "payments-1", "CrashLoopBackOff"),
		pod("boutique", "frontend-1", ""),
	)
	spec := map[string]any{
		"resource":        "pods",
		"name":            "payments-1",
		"jsonpath":        "{.status.containerStatuses[*].state.waiting.reason}",
		"expect_contains": "CrashLoopBackOff",
		"timeout":         "1s",
		"interval":        "10ms",
	}
	p := NewK8sProber(dyn, testMapper())
	res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res.Err != nil {
		t.Fatalf("Err: %v", res.Err)
	}
	if !res.Passed {
		t.Fatalf("named-object probe did not pass; observed %q", res.Observed)
	}
}

func TestK8sProbeTreatsMissingKeysAsEmptyRatherThanAnError(t *testing.T) {
	// A pod that has not crashed has no state.waiting.reason at all. If that
	// were an error, every probe would fail on its first poll for the wrong
	// reason and never get the chance to succeed.
	dyn := newFakeDyn(pod("boutique", "payments-1", ""))
	p := NewK8sProber(dyn, testMapper())
	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if res.Err != nil {
		t.Fatalf("missing key surfaced as an error: %v", res.Err)
	}
	if res.Passed {
		t.Fatal("probe passed against a healthy pod")
	}
	if res.Attempts < 2 {
		t.Errorf("Attempts=%d — the probe gave up instead of polling", res.Attempts)
	}
}

func TestK8sProbeDefaultsToTheFaultsNamespaceAndHonoursAnExplicitOne(t *testing.T) {
	dyn := newFakeDyn(pod("boutique", "payments-1", "CrashLoopBackOff"))
	var mu sync.Mutex
	var seen []string
	dyn.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		seen = append(seen, a.GetNamespace())
		mu.Unlock()
		return false, nil, nil
	})
	p := NewK8sProber(dyn, testMapper())

	if res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"}); !res.Passed {
		t.Fatalf("probe did not pass: %+v", res)
	}
	if len(seen) == 0 || seen[0] != "boutique" {
		t.Fatalf("namespaces read=%v, want the fault's own namespace first", seen)
	}

	spec := crashLoopSpec()
	spec["namespace"] = "other"
	_ = p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != "other" {
		t.Errorf("explicit namespace ignored: read %q", last)
	}
}

func TestK8sProbeWithoutAnyNamespaceSaysSo(t *testing.T) {
	p := NewK8sProber(newFakeDyn(), testMapper())
	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{})
	if res.Err == nil {
		t.Fatal("probe ran with no namespace at all")
	}
	if !strings.Contains(res.Err.Error(), "namespace") {
		t.Errorf("error %q does not mention the namespace", res.Err)
	}
}

func TestK8sProbePassesTheLabelSelectorThrough(t *testing.T) {
	dyn := newFakeDyn(pod("boutique", "payments-1", "CrashLoopBackOff"))
	var mu sync.Mutex
	var selectors []string
	dyn.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		la, ok := a.(k8stesting.ListActionImpl)
		if ok {
			mu.Lock()
			selectors = append(selectors, la.ListRestrictions.Labels.String())
			mu.Unlock()
		}
		return false, nil, nil
	})
	spec := crashLoopSpec()
	spec["label_selector"] = "app=payments"
	p := NewK8sProber(dyn, testMapper())
	if res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"}); !res.Passed {
		t.Fatalf("probe did not pass: %+v", res)
	}
	if len(selectors) == 0 || selectors[0] != "app=payments" {
		t.Errorf("label selectors seen=%v, want app=payments", selectors)
	}
}

func TestK8sProbeNamesAnUnknownResource(t *testing.T) {
	spec := crashLoopSpec()
	spec["resource"] = "widgets"
	p := NewK8sProber(newFakeDyn(), testMapper())
	res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res.Err == nil {
		t.Fatal("unknown resource accepted")
	}
	if !strings.Contains(res.Err.Error(), "widgets") {
		t.Errorf("error %q does not name the resource", res.Err)
	}
}

func TestK8sProbePollsThroughATransientReadFailure(t *testing.T) {
	dyn := newFakeDyn(pod("boutique", "payments-1", "CrashLoopBackOff"))
	var mu sync.Mutex
	calls := 0
	dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	p := NewK8sProber(dyn, testMapper())
	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if !res.Passed {
		t.Fatalf("a transient read error ended the probe: %+v", res)
	}
	if res.Err != nil {
		t.Errorf("Err=%v, want nil once a later poll succeeded", res.Err)
	}
}

func TestK8sProbeReportsAReadErrorThatNeverClears(t *testing.T) {
	dyn := newFakeDyn()
	dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("configmaps is forbidden")
	})
	p := NewK8sProber(dyn, testMapper())
	res := p.Run(context.Background(), k8sProbe(crashLoopSpec()), Target{Namespace: "boutique"})
	if res.Passed {
		t.Fatal("probe passed while unable to read anything")
	}
	// An unreadable probe is a different thing from an unsatisfied one, and
	// must not be reported as "the fault didn't land".
	if res.Err == nil {
		t.Fatal("Err is nil after every poll failed")
	}
	if !strings.Contains(res.Err.Error(), "forbidden") {
		t.Errorf("error %q loses the underlying cause", res.Err)
	}
}

func TestK8sProbeStopsWhenTheContextIsCancelled(t *testing.T) {
	spec := crashLoopSpec()
	spec["timeout"] = "1h" // would hang forever if cancellation were ignored
	dyn := newFakeDyn(pod("boutique", "payments-1", ""))
	p := NewK8sProber(dyn, testMapper())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	done := make(chan Result, 1)
	go func() { done <- p.Run(ctx, k8sProbe(spec), Target{Namespace: "boutique"}) }()
	select {
	case res := <-done:
		if res.Passed {
			t.Fatal("probe passed after cancellation")
		}
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("Err=%v, want context.Canceled", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe ignored context cancellation")
	}
}

func TestK8sProbeAcceptsAGroupQualifiedResource(t *testing.T) {
	empty := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata":  map[string]any{"name": "frontend-abc", "namespace": "boutique"},
		"endpoints": []any{},
	}}
	spec := map[string]any{
		"resource":     "endpointslices.discovery.k8s.io",
		"jsonpath":     "{.items[*].endpoints[*].addresses[*]}",
		"expect_empty": true,
		"timeout":      "1s",
		"interval":     "10ms",
	}
	p := NewK8sProber(newFakeDyn(empty), testMapper())
	res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res.Err != nil {
		t.Fatalf("group-qualified resource rejected: %v", res.Err)
	}
	if !res.Passed {
		t.Fatal("probe did not pass")
	}
}

func TestK8sProbeRejectsAnUnparseableJsonpath(t *testing.T) {
	spec := crashLoopSpec()
	spec["jsonpath"] = "{.items[*"
	p := NewK8sProber(newFakeDyn(), testMapper())
	res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"})
	if res.Err == nil {
		t.Fatal("malformed jsonpath accepted")
	}
	if res.Attempts != 0 {
		t.Errorf("Attempts=%d, want 0 — a bad expression should fail before polling", res.Attempts)
	}
}

func TestDefaultsAreUsedWhenTimingIsUnspecified(t *testing.T) {
	got, err := parseK8sSpec(map[string]any{
		"resource": "pods", "jsonpath": "{.x}", "expect_contains": "y",
	}, Target{Namespace: "boutique"})
	if err != nil {
		t.Fatalf("parseK8sSpec: %v", err)
	}
	if got.timeout != DefaultTimeout {
		t.Errorf("timeout=%s, want %s", got.timeout, DefaultTimeout)
	}
	if got.interval != DefaultInterval {
		t.Errorf("interval=%s, want %s", got.interval, DefaultInterval)
	}
	if got.namespace != "boutique" {
		t.Errorf("namespace=%q, want the fallback", got.namespace)
	}
}

func TestListMetadataIsReachableFromAJsonpath(t *testing.T) {
	// Guards the {.items[*]...} contract: the object handed to jsonpath must
	// be the whole list, not the bare item slice.
	dyn := newFakeDyn(pod("boutique", "payments-1", "CrashLoopBackOff"))
	spec := map[string]any{
		"resource":        "pods",
		"jsonpath":        "{.items[*].metadata.name}",
		"expect_contains": "payments-1",
		"timeout":         "1s",
		"interval":        "10ms",
	}
	p := NewK8sProber(dyn, testMapper())
	if res := p.Run(context.Background(), k8sProbe(spec), Target{Namespace: "boutique"}); !res.Passed {
		t.Fatalf("items[*] did not resolve: %+v", res)
	}
}
