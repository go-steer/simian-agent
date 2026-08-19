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
	"errors"
	"reflect"
	"testing"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// nsOf returns the namespaces a selector node names, as applied by the driver.
func nsOf(t *testing.T, node any) []string {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("expected selector node, got %T", node)
	}
	arr, ok := m["namespaces"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, v.(string))
	}
	return out
}

func TestSpecNamespaceScope(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]any
		// wantReject is true when the manifest must never reach the driver.
		wantReject bool
		// wantNarrowed is the set of dotted paths the executor pinned.
		wantNarrowed []string
		// check runs against the spec the driver actually received.
		check func(t *testing.T, spec map[string]any)
	}{
		{
			name: "selector naming only the target namespace is accepted untouched",
			spec: map[string]any{
				"action": "pod-kill",
				"selector": map[string]any{
					"namespaces":     []any{"online-boutique"},
					"labelSelectors": map[string]any{"app": "paymentservice"},
				},
			},
			check: func(t *testing.T, spec map[string]any) {
				if got := nsOf(t, spec["selector"]); !reflect.DeepEqual(got, []string{"online-boutique"}) {
					t.Errorf("selector.namespaces=%v, want [online-boutique]", got)
				}
			},
		},
		{
			name: "selector escaping the arena is rejected even though the target is eligible",
			spec: map[string]any{
				"action":   "pod-kill",
				"selector": map[string]any{"namespaces": []any{"kube-system"}},
			},
			wantReject: true,
		},
		{
			name: "one ineligible namespace poisons an otherwise eligible list",
			spec: map[string]any{
				"action":   "pod-kill",
				"selector": map[string]any{"namespaces": []any{"online-boutique", "kube-system"}},
			},
			wantReject: true,
		},
		{
			name: "unscoped selector is narrowed to the target namespace",
			spec: map[string]any{
				"action":   "delay",
				"selector": map[string]any{"labelSelectors": map[string]any{"app": "paymentservice"}},
			},
			wantNarrowed: []string{"spec.selector"},
			check: func(t *testing.T, spec map[string]any) {
				if got := nsOf(t, spec["selector"]); !reflect.DeepEqual(got, []string{"online-boutique"}) {
					t.Errorf("selector.namespaces=%v, want [online-boutique]", got)
				}
			},
		},
		{
			name: "empty selector is narrowed rather than left cluster-wide",
			spec: map[string]any{
				"action":   "pod-kill",
				"selector": map[string]any{},
			},
			wantNarrowed: []string{"spec.selector"},
			check: func(t *testing.T, spec map[string]any) {
				if got := nsOf(t, spec["selector"]); !reflect.DeepEqual(got, []string{"online-boutique"}) {
					t.Errorf("selector.namespaces=%v, want [online-boutique]", got)
				}
			},
		},
		{
			name: "NetworkChaos target selector is checked, not just the primary selector",
			spec: map[string]any{
				"action":    "partition",
				"direction": "both",
				"selector":  map[string]any{"namespaces": []any{"online-boutique"}},
				"target": map[string]any{
					"mode":     "all",
					"selector": map[string]any{"namespaces": []any{"kube-system"}},
				},
			},
			wantReject: true,
		},
		{
			name: "unscoped nested target selector is narrowed too",
			spec: map[string]any{
				"action":   "partition",
				"selector": map[string]any{"namespaces": []any{"online-boutique"}},
				"target": map[string]any{
					"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
				},
			},
			wantNarrowed: []string{"spec.target.selector"},
			check: func(t *testing.T, spec map[string]any) {
				tgt := spec["target"].(map[string]any)
				if got := nsOf(t, tgt["selector"]); !reflect.DeepEqual(got, []string{"online-boutique"}) {
					t.Errorf("target.selector.namespaces=%v, want [online-boutique]", got)
				}
			},
		},
		{
			name: "pods map keys are namespaces and are checked as such",
			spec: map[string]any{
				"action": "pod-kill",
				"selector": map[string]any{
					"pods": map[string]any{"kube-system": []any{"etcd-0"}},
				},
			},
			wantReject: true,
		},
		{
			name: "eligible pods map is accepted and not narrowed",
			spec: map[string]any{
				"action": "pod-kill",
				"selector": map[string]any{
					"pods": map[string]any{"online-boutique": []any{"paymentservice-0"}},
				},
			},
			check: func(t *testing.T, spec map[string]any) {
				sel := spec["selector"].(map[string]any)
				if _, ok := sel["namespaces"]; ok {
					t.Error("selector already named a namespace via pods; must not be narrowed")
				}
			},
		},
		{
			name: "a label literally named namespaces is not mistaken for a selector",
			spec: map[string]any{
				"action": "pod-kill",
				"selector": map[string]any{
					"namespaces":     []any{"online-boutique"},
					"labelSelectors": map[string]any{"namespaces": "kube-system"},
				},
			},
			check: func(t *testing.T, spec map[string]any) {
				sel := spec["selector"].(map[string]any)
				ls := sel["labelSelectors"].(map[string]any)
				if got := ls["namespaces"]; got != "kube-system" {
					t.Errorf("labelSelectors.namespaces=%v, want it left alone", got)
				}
			},
		},
		{
			name: "a spec with no selector at all is left alone",
			spec: map[string]any{
				"action": "delay",
				"delay":  map[string]any{"latency": "250ms"},
			},
			check: func(t *testing.T, spec map[string]any) {
				if _, ok := spec["selector"]; ok {
					t.Error("executor invented a selector; it must only narrow ones that exist")
				}
			},
		},
		{
			name: "containerSelector is scoped like any other selector",
			spec: map[string]any{
				"action":            "container-kill",
				"selector":          map[string]any{"namespaces": []any{"online-boutique"}},
				"containerSelector": map[string]any{"namespaces": []any{"kube-system"}},
			},
			wantReject: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec, driver, auditor := newTestExecutor(t, DefaultConfig(),
				map[string]bool{"online-boutique": true}, nil)
			m := goodManifest()
			m.Spec = tc.spec

			_, err := exec.Apply(context.Background(), m)

			if tc.wantReject {
				if err == nil {
					t.Fatal("expected rejection")
				}
				var ee *simian.ExecutorError
				if !errors.As(err, &ee) {
					t.Fatalf("expected *ExecutorError, got %T: %v", err, err)
				}
				if ee.Stage != simian.StageSafety || ee.Reason != simian.ReasonNamespaceNotEligible {
					t.Errorf("got %s/%s, want safety/%s", ee.Stage, ee.Reason, simian.ReasonNamespaceNotEligible)
				}
				if got := len(driver.AppliedCopy()); got != 0 {
					t.Errorf("driver.Applied=%d, want 0 (rejection must never reach the driver)", got)
				}
				ev, ok := auditor.FindEvent(audit.EventExecutorRejected)
				if !ok {
					t.Fatal("expected executor.rejected audit event")
				}
				if ev.Reason != string(simian.ReasonNamespaceNotEligible) {
					t.Errorf("audited reason=%q, want %q", ev.Reason, simian.ReasonNamespaceNotEligible)
				}
				return
			}

			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			applied := driver.AppliedCopy()
			if len(applied) != 1 {
				t.Fatalf("driver.Applied=%d, want 1", len(applied))
			}
			if tc.check != nil {
				tc.check(t, applied[0].Spec)
			}

			ev, ok := auditor.FindEvent(audit.EventExecutorValidated)
			if !ok {
				t.Fatal("expected executor.validated audit event")
			}
			var gotNarrowed []string
			if ev.Payload != nil {
				if p, ok := ev.Payload["selector_paths"].([]string); ok {
					gotNarrowed = p
				}
			}
			if !reflect.DeepEqual(gotNarrowed, tc.wantNarrowed) {
				t.Errorf("narrowed paths=%v, want %v", gotNarrowed, tc.wantNarrowed)
			}
		})
	}
}

// The scope check exists because Targets does not bound a Chaos Mesh fault.
// Engines whose driver builds its own objects from Targets are exempt, and
// must stay exempt only for as long as that remains true.
func TestSpecNamespaceScopeSkipsExemptEngines(t *testing.T) {
	for engine := range specScopeExempt {
		t.Run(string(engine), func(t *testing.T) {
			exec, _, _ := newTestExecutor(t, DefaultConfig(),
				map[string]bool{"online-boutique": true}, nil)
			m := goodManifest()
			m.Engine = engine
			m.Spec = map[string]any{"selector": map[string]any{"namespaces": []any{"kube-system"}}}

			narrowed, err := exec.validateSpecNamespaceScope(context.Background(), &m)
			if err != nil {
				t.Fatalf("exempt engine should not be scope-checked: %v", err)
			}
			if narrowed != nil {
				t.Errorf("narrowed=%v, want nil", narrowed)
			}
		})
	}
}

func TestFindSelectorSitesOrderIsDeterministic(t *testing.T) {
	spec := map[string]any{
		"selector": map[string]any{"namespaces": []any{"a"}},
		"target":   map[string]any{"selector": map[string]any{"namespaces": []any{"b"}}},
		"containerSelector": map[string]any{
			"namespaces": []any{"c"},
		},
	}
	want := []string{"spec.containerSelector", "spec.selector", "spec.target.selector"}
	for i := 0; i < 20; i++ {
		var got []string
		for _, s := range findSelectorSites(spec) {
			got = append(got, s.path)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: paths=%v, want %v", i, got, want)
		}
	}
}
