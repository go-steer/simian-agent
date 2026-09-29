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

package chaosmesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ simian.InjectionConfirmer = (*Driver)(nil)

// Status shapes below are copied from Chaos Mesh 2.8.4 on kind, trimmed.
func cond(pairs ...string) []any {
	var out []any
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"type": pairs[i], "status": pairs[i+1]})
	}
	return out
}

func record(id string, events ...[2]string) map[string]any {
	var evs []any
	for _, e := range events {
		ev := map[string]any{"operation": "Apply", "type": e[0]}
		if e[1] != "" {
			ev["message"] = e[1]
		}
		evs = append(evs, ev)
	}
	return map[string]any{"id": id, "events": evs}
}

func chaosObj(status map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/v1alpha1",
		"kind":       "HTTPChaos",
		"metadata":   map[string]any{"name": "simian-abc", "namespace": "bank"},
	}}
	if status != nil {
		obj.Object["status"] = status
	}
	return obj
}

func TestInjectionStateReadsChaosMeshStatus(t *testing.T) {
	const tooLong = "toda startup takes too long or an error occurs: No such file or directory"
	for _, tc := range []struct {
		name     string
		status   map[string]any
		wantDone bool
		wantSays string
	}{
		{
			name: "all injected",
			status: map[string]any{
				"conditions": cond("Selected", "True", "AllInjected", "True", "AllRecovered", "False"),
				"experiment": map[string]any{"containerRecords": []any{
					record("bank/web-1", [2]string{"Succeeded", ""}),
				}},
			},
			wantDone: true,
			wantSays: "AllInjected=True",
		},
		{
			// Retrying is normal: a replacement pod's container is not there
			// yet, the controller tries again, and then it works.
			name: "failed a few times, then succeeded",
			status: map[string]any{
				"conditions": cond("Selected", "True", "AllInjected", "False"),
				"experiment": map[string]any{"containerRecords": []any{
					record("bank/web-1/nginx",
						[2]string{"Failed", "container not found"},
						[2]string{"Failed", "container not found"},
						[2]string{"Succeeded", ""}),
				}},
			},
			wantDone: true,
			wantSays: "successful apply",
		},
		{
			// A fault shorter than the polling interval injects and recovers
			// between two reads. AllInjected is False again by then.
			name: "injected and already recovered",
			status: map[string]any{
				"conditions": cond("Selected", "True", "AllInjected", "False", "AllRecovered", "True"),
				"experiment": map[string]any{"containerRecords": []any{
					record("bank/userservice-1/userservice", [2]string{"Succeeded", ""}),
				}},
			},
			wantDone: true,
		},
		{
			name: "failing on every attempt",
			status: map[string]any{
				"conditions": cond("Selected", "True", "AllInjected", "False"),
				"experiment": map[string]any{"containerRecords": []any{
					record("bank/web-1/nginx", [2]string{"Failed", tooLong}, [2]string{"Failed", tooLong}),
				}},
			},
			wantSays: "0 of 1 target(s) injected; bank/web-1/nginx: " + tooLong,
		},
		{
			name: "one of two targets never takes",
			status: map[string]any{
				"conditions": cond("Selected", "True", "AllInjected", "False"),
				"experiment": map[string]any{"containerRecords": []any{
					record("bank/web-1", [2]string{"Succeeded", ""}),
					record("bank/web-2", [2]string{"Failed", "resolv.conf.chaos.bak: No such file"}),
				}},
			},
			wantSays: "1 of 2 target(s) injected; bank/web-2: resolv.conf.chaos.bak",
		},
		{
			name:     "selector matched nothing",
			status:   map[string]any{"conditions": cond("Selected", "False", "AllInjected", "False")},
			wantSays: "Selected=False",
		},
		{
			name:     "not reconciled yet",
			status:   nil,
			wantSays: "has not reconciled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, says := injectionState(chaosObj(tc.status))
			if done != tc.wantDone {
				t.Errorf("done = %v, want %v (observed %q)", done, tc.wantDone, says)
			}
			if !strings.Contains(says, tc.wantSays) {
				t.Errorf("observed = %q, want it to contain %q", says, tc.wantSays)
			}
		})
	}
}

// createWithStatus puts a chaos object in the fake cluster as Chaos Mesh
// would leave it, and returns its engine UID.
func createWithStatus(t *testing.T, d *Driver, status map[string]any) string {
	t.Helper()
	obj := chaosObj(status)
	if _, err := d.dyn.Resource(httpChaosGVR).Namespace("bank").Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return engineUID("bank", "simian-abc", httpChaosGVR)
}

func TestConfirmInjectedReturnsOnceChaosMeshSaysSo(t *testing.T) {
	d, _ := newTestDriver(t)
	uid := createWithStatus(t, d, map[string]any{
		"conditions": cond("Selected", "True", "AllInjected", "True"),
	})

	observed, err := d.ConfirmInjected(context.Background(), uid)
	if err != nil {
		t.Fatalf("ConfirmInjected: %v", err)
	}
	if !strings.Contains(observed, "AllInjected=True") {
		t.Errorf("observed = %q", observed)
	}
}

// The failure has to carry Chaos Mesh's own message. "Not injected" alone is
// what the trial's audit log would have said with this check in place, and it
// would have taken a kubectl describe on an object that no longer exists to
// learn why.
func TestConfirmInjectedGivesUpWithTheEnginesReason(t *testing.T) {
	d, _ := newTestDriver(t)
	d.confirmInterval = 10 * time.Millisecond
	d.WithConfirmTimeout(50 * time.Millisecond)
	uid := createWithStatus(t, d, map[string]any{
		"conditions": cond("Selected", "True", "AllInjected", "False"),
		"experiment": map[string]any{"containerRecords": []any{
			record("bank/web-1/nginx", [2]string{"Failed", "ls: cannot access '/etc/resolv.conf.chaos.bak'"}),
		}},
	})

	observed, err := d.ConfirmInjected(context.Background(), uid)
	if err == nil {
		t.Fatal("ConfirmInjected succeeded on a fault that failed every attempt")
	}
	for _, s := range []string{err.Error(), observed} {
		if !strings.Contains(s, "resolv.conf.chaos.bak") {
			t.Errorf("%q does not carry the engine's message", s)
		}
	}
	if !strings.Contains(err.Error(), "bank/simian-abc") {
		t.Errorf("error does not name the object: %v", err)
	}
}

func TestConfirmInjectedReportsAnObjectThatIsGone(t *testing.T) {
	d, _ := newTestDriver(t)
	d.confirmInterval = 10 * time.Millisecond
	d.WithConfirmTimeout(50 * time.Millisecond)

	_, err := d.ConfirmInjected(context.Background(), engineUID("bank", "simian-gone", httpChaosGVR))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found explanation", err)
	}
}
