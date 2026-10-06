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

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestShortUIDTrimsLongIDs(t *testing.T) {
	if got := shortUID("f-01KX42VR47D5XZEH65N3BP1VC6"); got != "f-01KX42VR47" {
		t.Errorf("shortUID long: got %q, want %q", got, "f-01KX42VR47")
	}
	if got := shortUID("short"); got != "short" {
		t.Errorf("shortUID short: got %q, want %q", got, "short")
	}
}

func TestFormatRemainingHandlesExpiredAndFuture(t *testing.T) {
	if got := formatRemaining(-5 * time.Second); got != "expired" {
		t.Errorf("negative duration: got %q, want %q", got, "expired")
	}
	if got := formatRemaining(30 * time.Second); got != "30s" {
		t.Errorf("30s: got %q, want %q", got, "30s")
	}
}

func TestRenderSnapshotEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderSnapshot(&buf, &watchSnapshot{
		Namespace:  "boutique-m3",
		CapturedAt: time.Date(2026, 7, 17, 15, 4, 5, 0, time.UTC),
	})
	out := buf.String()
	for _, want := range []string{"boutique-m3", "15:04:05", "ACTIVE FAULTS (0)", "RECENT FAULTS (0)", "Ctrl-C"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q; got:\n%s", want, out)
		}
	}
}

func TestRenderSnapshotWithFaults(t *testing.T) {
	now := time.Date(2026, 7, 17, 15, 4, 5, 0, time.UTC)
	var af activeFault
	af.FaultUID = "f-01KX42VR47D5XZEH65N3BP1VC6"
	af.Manifest.Engine = "envoy-fault"
	af.Manifest.ResourceKind = "EnvoyHttpDelay"
	af.Manifest.Targets = []struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}{{Namespace: "boutique-m3", Name: "frontend"}}
	af.Deadline = now.Add(15 * time.Second)

	var rf recentFault
	rf.FaultUID = "f-01OLDID"
	rf.Manifest.Engine = "network-policy"
	rf.Manifest.ResourceKind = "NetworkPolicy"
	rf.Manifest.Targets = []struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}{{Namespace: "boutique-m3", Name: "cartservice"}}
	rf.AppliedAt = now.Add(-60 * time.Second)
	rf.ClearedAt = now.Add(-30 * time.Second)
	rf.ClearReason = "deadline-reached"

	var buf bytes.Buffer
	renderSnapshot(&buf, &watchSnapshot{
		Namespace:  "boutique-m3",
		CapturedAt: now,
		Active:     []activeFault{af},
		Recent:     []recentFault{rf},
	})
	out := buf.String()
	for _, want := range []string{
		"ACTIVE FAULTS (1)",
		"f-01KX42VR47", // truncated
		"envoy-fault",
		"EnvoyHttpDelay",
		"frontend",
		"15s remaining",
		"RECENT FAULTS (1)",
		"network-policy",
		"NetworkPolicy",
		"cartservice",
		"cleared (deadline-reached)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q; got:\n%s", want, out)
		}
	}
}

// get_recent_faults has always wrapped its list; watch read it as a bare list
// and failed every poll. Both shapes are accepted now.
func TestWatchParsesTheToolsWrappedLists(t *testing.T) {
	recent := `{"enabled":true,"recent":[{"fault_uid":"f-1","manifest":{"engine":"chaos-mesh","resource_kind":"PodChaos","targets":[{"namespace":"boutique","name":"cartservice"}]}}]}`
	cycles := `{"enabled":true,"cycles":[{"namespace":"boutique","outcome":"skipped","reason":"health-gate","detail":"workload Deployment/adservice: 0/1 pods ready"},{"namespace":"boutique","outcome":"completed","hypothesis":"checkout survives a cartservice restart","steps":[{"order":1,"kind":"PodChaos","target":"cartservice","duration":"1m0s","duration_rationale":"recovery takes a minute"}],"applied":["f-1"]}]}`
	snap, err := parseSnapshot("boutique", "null", recent, cycles)
	if err != nil {
		t.Fatalf("parseSnapshot: %v", err)
	}
	if len(snap.Recent) != 1 || snap.Recent[0].FaultUID != "f-1" || len(snap.Cycles) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if bare, err := parseSnapshot("boutique", "", `[{"fault_uid":"f-2"}]`, ""); err != nil || len(bare.Recent) != 1 {
		t.Errorf("bare list: %+v, %v", bare, err)
	}

	var buf bytes.Buffer
	renderSnapshot(&buf, snap)
	out := buf.String()
	for _, want := range []string{
		"AUTONOMOUS CYCLES (2)",
		"checkout survives a cartservice restart",
		"1. PodChaos → cartservice for 1m0s",
		"why that long: recovery takes a minute",
		"skipped   health-gate: workload Deployment/adservice",
		"completed PodChaos→cartservice (1 applied)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output lacks %q:\n%s", want, out)
		}
	}
}
