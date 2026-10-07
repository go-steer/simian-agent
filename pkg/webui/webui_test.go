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

package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/topology"
)

type fakeActive []simian.ActiveFault

func (f fakeActive) ListActive(_ context.Context, ns string) ([]simian.ActiveFault, error) {
	var out []simian.ActiveFault
	for _, a := range f {
		if a.Manifest.TargetsNamespace(ns) {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeTopology struct{}

func (fakeTopology) Snapshot(context.Context, string) (*topology.TargetTopology, error) {
	return &topology.TargetTopology{
		Workloads: []topology.Workload{{Kind: "Deployment", Name: "frontend", DesiredReplicas: 2}},
		PodStatus: map[string][]topology.PodSummary{"frontend": {{Ready: true}, {Ready: false}}},
	}, nil
}

func newServer(t *testing.T) (*httptest.Server, *Broadcaster) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := NewBroadcaster(ctx)
	cycles := audit.NewCycleLog(0)
	cycles.Add(audit.Record{TS: time.Now(), Event: audit.EventCycleStarted, Payload: map[string]any{"namespace": "boutique"}})
	cycles.Add(audit.Record{TS: time.Now(), Event: audit.EventCycleSkipped, Reason: "budget-full", Payload: map[string]any{"namespace": "boutique"}})
	faults := audit.NewFaultLog(0)
	faults.Add(audit.Record{TS: time.Now(), Event: audit.EventExecutorReceived, FaultUID: "f-1",
		Payload: map[string]any{"kind": "PodChaos", "targets": []any{map[string]any{"namespace": "boutique", "name": "cartservice"}}}})
	srv := httptest.NewServer(Handler(Deps{
		Version: "0.3.0-test",
		Active: fakeActive{{FaultUID: "f-1", Deadline: time.Now().Add(time.Minute),
			Manifest: simian.FaultManifest{ResourceKind: "PodChaos", Targets: []simian.TargetRef{{Namespace: "boutique", Name: "cartservice"}}}}},
		Faults: faults, Cycles: cycles, Topology: fakeTopology{},
		Arenas:     func(context.Context) ([]string, error) { return []string{"bank", "boutique"}, nil },
		Autonomous: []string{"boutique"}, Events: events,
	}))
	t.Cleanup(srv.Close)
	return srv, events
}

func getJSON(t *testing.T, srv *httptest.Server, path string, v any) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestTheAPIServesWhatThePageReads(t *testing.T) {
	srv, _ := newServer(t)
	var info struct {
		Version    string   `json:"version"`
		Autonomous []string `json:"autonomous"`
	}
	getJSON(t, srv, "/api/info", &info)
	if info.Version != "0.3.0-test" || len(info.Autonomous) != 1 {
		t.Errorf("info = %+v", info)
	}
	var arenas []string
	getJSON(t, srv, "/api/arenas", &arenas)
	if strings.Join(arenas, ",") != "bank,boutique" {
		t.Errorf("arenas = %v", arenas)
	}
	var active []simian.ActiveFault
	getJSON(t, srv, "/api/active?namespace=bank", &active)
	if len(active) != 0 {
		t.Errorf("bank active = %+v, want none", active)
	}
	getJSON(t, srv, "/api/active?namespace=boutique", &active)
	if len(active) != 1 || active[0].FaultUID != "f-1" {
		t.Errorf("boutique active = %+v", active)
	}
	var faults []audit.FaultRow
	getJSON(t, srv, "/api/faults?namespace=boutique", &faults)
	if len(faults) != 1 || faults[0].Kind != "PodChaos" {
		t.Errorf("faults = %+v", faults)
	}
	var cycles []audit.CycleRow
	getJSON(t, srv, "/api/cycles?namespace=boutique", &cycles)
	if len(cycles) != 1 || cycles[0].Reason != "budget-full" {
		t.Errorf("cycles = %+v", cycles)
	}
	var wls []workload
	getJSON(t, srv, "/api/topology?namespace=boutique", &wls)
	if len(wls) != 1 || wls[0].Ready != 1 || wls[0].Desired != 2 {
		t.Errorf("workloads = %+v", wls)
	}
}

func TestThePageIsServed(t *testing.T) {
	srv, _ := newServer(t)
	for path, want := range map[string]string{"/ui/": "<title>Simian</title>", "/ui/app.js": "EventSource", "/ui/style.css": "--bad"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("%s: %s, body lacks %q", path, resp.Status, want)
		}
	}
}

// An audit event the controller emits reaches an open browser stream.
func TestTheEventStreamCarriesAuditEvents(t *testing.T) {
	srv, events := newServer(t)
	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Scan() // ": connected"
	go func() {
		time.Sleep(100 * time.Millisecond) // the subscription is registered by now
		events.Emit(context.Background(), simian.AuditEvent{Event: "fault.recovered", FaultUID: "f-1", Payload: map[string]any{"passed": false}})
	}()
	deadline := time.After(5 * time.Second)
	got := make(chan string, 1)
	go func() {
		for lines.Scan() {
			if strings.HasPrefix(lines.Text(), "data: ") {
				got <- strings.TrimPrefix(lines.Text(), "data: ")
				return
			}
		}
	}()
	select {
	case data := <-got:
		var e Event
		if err := json.Unmarshal([]byte(data), &e); err != nil || e.Event != "fault.recovered" || e.Payload["passed"] != false {
			t.Errorf("event = %s (%v)", data, err)
		}
	case <-deadline:
		t.Fatal("no event on the stream")
	}
}

// A browser that connects late still sees what happened before it did.
func TestANewStreamOpensWithRecentEvents(t *testing.T) {
	srv, events := newServer(t)
	events.Emit(context.Background(), simian.AuditEvent{Event: "cycle.started", Payload: map[string]any{"namespace": "boutique"}})
	time.Sleep(100 * time.Millisecond)
	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewScanner(resp.Body)
	got := make(chan string, 1)
	go func() {
		for lines.Scan() {
			if strings.HasPrefix(lines.Text(), "data: ") {
				got <- lines.Text()
				return
			}
		}
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, `"cycle.started"`) {
			t.Errorf("replayed %s", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream opened without the earlier event")
	}
}
