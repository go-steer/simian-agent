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

// Package webui serves Simian's web UI from the controller: a read-only page
// at /ui/, the small JSON API under /api/ it reads, and a live stream of
// audit events at /api/events.
//
// It is a view over the same records as simian watch, simian audit export
// and the metrics — the controller's lease registry and its fault and cycle
// logs — so the page cannot disagree with them. A JSON API rather than MCP
// from the browser: the data is the same, and plain fetch and EventSource are
// what a page without a build step can use cleanly. Agents keep the MCP
// endpoint.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/topology"
)

//go:embed web
var webFS embed.FS

// Deps are what the UI reads. Any may be nil; its panel then says so.
type Deps struct {
	Version string
	// Name is how this controller is labelled in a UI that watches several,
	// e.g. its cluster; empty leaves the UI to use the host.
	Name string
	// AllowedOrigins are the pages, other than this controller's own, that
	// may call it from the browser with the user's credentials.
	AllowedOrigins []string
	// Controllers are the other Simians this deployment offers in the
	// page's controller list (GET /api/controllers).
	Controllers []Controller
	// Active lists the faults held right now. *executor.Executor.
	Active interface {
		ListActive(ctx context.Context, namespace string) ([]simian.ActiveFault, error)
	}
	Faults interface {
		Recent(namespace string, limit int) []audit.FaultRow
	}
	Cycles interface {
		Recent(namespace string, limit int) []audit.CycleRow
	}
	Topology interface {
		Snapshot(ctx context.Context, namespace string) (*topology.TargetTopology, error)
	}
	// Arenas resolves the namespaces Simian may act on.
	Arenas func(ctx context.Context) ([]string, error)
	// Autonomous is the namespaces the autonomous loop runs in; empty when
	// it is off.
	Autonomous []string
	Events     *Broadcaster

	// Auth decides who a request is from and whether they may write. Nil is
	// NoAuth: read-only.
	Auth Authenticator
	// Executor applies and clears faults submitted from the page; nil turns
	// the write endpoints off. *executor.Executor.
	Executor interface {
		Apply(ctx context.Context, m simian.FaultManifest) (string, error)
		Clear(ctx context.Context, faultUID string) error
	}
	// Catalog lists the fault kinds the engines offer, for the submit form.
	Catalog func(ctx context.Context) ([]simian.CatalogEntry, error)
	// Translate turns a plain-English request into a manifest (the LLM);
	// nil turns the intent mode off.
	Translate func(ctx context.Context, intent, namespace string, duration time.Duration) (simian.FaultManifest, error)

	// Install is the controller's limits, shown on the configuration panel.
	Install Install
	// Excluded lists an arena's excluded workloads.
	Excluded func(ctx context.Context, namespace string) ([]string, error)
	// Autonomy lets admins configure, pause and resume autonomous mode;
	// Limits are the ceilings their settings must fit under.
	Autonomy *Autonomy
	Limits   func(ctx context.Context) (loop.Limits, error)
	// Auditor records admin changes.
	Auditor simian.Auditor
}

// Handler serves /ui/ and /api/.
func Handler(d Deps) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServerFS(static)))
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"version": d.Version, "name": d.Name, "autonomous": nonNil(d.autonomousNow())})
	})
	mux.HandleFunc("GET /api/arenas", func(w http.ResponseWriter, r *http.Request) {
		if d.Arenas == nil {
			writeJSON(w, []string{})
			return
		}
		ns, err := d.Arenas(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		sort.Strings(ns)
		writeJSON(w, nonNil(ns))
	})
	mux.HandleFunc("GET /api/active", func(w http.ResponseWriter, r *http.Request) {
		if d.Active == nil {
			writeJSON(w, []simian.ActiveFault{})
			return
		}
		faults, err := d.Active.ListActive(r.Context(), r.URL.Query().Get("namespace"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, nonNil(faults))
	})
	mux.HandleFunc("GET /api/faults", func(w http.ResponseWriter, r *http.Request) {
		if d.Faults == nil {
			writeJSON(w, []audit.FaultRow{})
			return
		}
		writeJSON(w, nonNil(d.Faults.Recent(r.URL.Query().Get("namespace"), limit(r, 50))))
	})
	mux.HandleFunc("GET /api/cycles", func(w http.ResponseWriter, r *http.Request) {
		if d.Cycles == nil {
			writeJSON(w, []audit.CycleRow{})
			return
		}
		writeJSON(w, nonNil(d.Cycles.Recent(r.URL.Query().Get("namespace"), limit(r, 20))))
	})
	mux.HandleFunc("GET /api/topology", func(w http.ResponseWriter, r *http.Request) {
		ns := r.URL.Query().Get("namespace")
		if d.Topology == nil || ns == "" {
			writeJSON(w, map[string]any{})
			return
		}
		snap, err := d.Topology.Snapshot(r.Context(), ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, workloadsOf(snap))
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) { d.Events.serve(w, r) })
	controllersRoute(mux, d.Controllers)
	writeRoutes(mux, d)
	adminRoutes(mux, d)

	auth := d.Auth
	if auth == nil {
		auth = NoAuth{}
	}
	top := http.NewServeMux()
	// For the load balancer's health check, which reaches the pod without
	// passing through IAP. It says nothing about the controller's state.
	top.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	top.Handle("/", authenticated(auth, mux))
	return withCORS(d.AllowedOrigins, top)
}

// autonomousNow is the namespaces autonomous mode runs in now: from the
// admin controls when there are any, else as started.
func (d Deps) autonomousNow() []string {
	if d.Autonomy == nil {
		return d.Autonomous
	}
	s, _ := d.Autonomy.Control.Get()
	if !s.Enabled {
		return nil
	}
	return s.Namespaces
}

// workload is what the topology panel shows: how many of a workload's pods
// are Ready against how many it wants.
type workload struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Desired int32  `json:"desired"`
	Ready   int    `json:"ready"`
	Pods    int    `json:"pods"`
}

func workloadsOf(snap *topology.TargetTopology) []workload {
	out := []workload{}
	if snap == nil {
		return out
	}
	for _, wl := range snap.Workloads {
		w := workload{Kind: wl.Kind, Name: wl.Name, Desired: wl.DesiredReplicas}
		for _, p := range snap.PodStatus[wl.Name] {
			w.Pods++
			if p.Ready {
				w.Ready++
			}
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func limit(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		return n
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Event is an audit event as the stream sends it.
type Event struct {
	TS       time.Time      `json:"ts"`
	Event    string         `json:"event"`
	FaultUID string         `json:"fault_uid,omitempty"`
	PlanID   string         `json:"plan_id,omitempty"`
	Mode     string         `json:"mode,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// Broadcaster fans audit events out to the UI's live streams. A
// simian.Auditor; a slow browser loses events rather than holding up the
// controller.
type Broadcaster struct {
	subs chan subOp
	in   chan Event
}

type subOp struct {
	ch  chan Event
	add bool
}

// NewBroadcaster starts the fan-out; it runs until ctx is done.
func NewBroadcaster(ctx context.Context) *Broadcaster {
	b := &Broadcaster{subs: make(chan subOp), in: make(chan Event, 256)}
	go b.run(ctx)
	return b
}

// replayed is how many recent events a newly connected browser is sent, so
// the feed opens with history rather than empty.
const replayed = 200

func (b *Broadcaster) run(ctx context.Context) {
	subs := map[chan Event]bool{}
	var recent []Event
	for {
		select {
		case <-ctx.Done():
			for ch := range subs {
				close(ch)
			}
			return
		case op := <-b.subs:
			if op.add {
				subs[op.ch] = true
				for _, e := range recent {
					select {
					case op.ch <- e:
					default:
					}
				}
			} else if subs[op.ch] {
				delete(subs, op.ch)
				close(op.ch)
			}
		case e := <-b.in:
			recent = append(recent, e)
			if len(recent) > replayed {
				recent = recent[len(recent)-replayed:]
			}
			for ch := range subs {
				select {
				case ch <- e:
				default: // this browser is behind; it catches up on its next poll
				}
			}
		}
	}
}

// Emit implements simian.Auditor.
func (b *Broadcaster) Emit(_ context.Context, e simian.AuditEvent) {
	var payload map[string]any
	if raw, err := json.Marshal(e.Payload); err == nil {
		_ = json.Unmarshal(raw, &payload)
	}
	ev := Event{TS: time.Now().UTC(), Event: e.Event, FaultUID: e.FaultUID, PlanID: e.PlanID,
		Mode: string(e.Mode), Reason: e.Reason, Payload: payload}
	select {
	case b.in <- ev:
	default: // the fan-out is behind; the event is still in the audit trail
	}
}

func (b *Broadcaster) serve(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if b == nil || !ok {
		http.Error(w, "event stream unavailable", http.StatusNotImplemented)
		return
	}
	ch := make(chan Event, replayed+64)
	b.subs <- subOp{ch: ch, add: true}
	defer func() {
		select {
		case b.subs <- subOp{ch: ch}:
		case <-time.After(time.Second):
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			raw, _ := json.Marshal(e)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}
