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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Install is how the controller was started: the limits every fault is held
// to, which the UI shows and cannot change.
type Install struct {
	MaxConcurrentFaults int                      `json:"max_concurrent_faults"`
	DurationCeiling     string                   `json:"duration_ceiling"`
	MinCooldown         string                   `json:"min_cooldown"`
	PermittedTiers      []simian.BlastRadiusTier `json:"permitted_tiers"`
	DefaultProbes       bool                     `json:"default_probes"`
	LLM                 string                   `json:"llm,omitempty"`
}

// Autonomy is what the admin controls drive. Nil leaves them off.
type Autonomy struct {
	Control *loop.Control
	// Store keeps a change across restarts; nil keeps it in memory only.
	Store interface {
		Save(ctx context.Context, settings loop.Settings, src loop.Source) error
		Clear(ctx context.Context) error
	}
}

type arenaInfo struct {
	Name     string   `json:"name"`
	Excluded []string `json:"excluded"`
}

type autonomousView struct {
	Settings loop.Settings `json:"settings"`
	Source   loop.Source   `json:"source"`
	Defaults loop.Settings `json:"defaults"`
}

type configView struct {
	Version    string          `json:"version"`
	Executor   Install         `json:"executor"`
	Arenas     []arenaInfo     `json:"arenas"`
	Autonomous *autonomousView `json:"autonomous,omitempty"`
	UI         map[string]any  `json:"ui"`
	You        Identity        `json:"you"`
}

func adminRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		id := identityFrom(r.Context())
		v := configView{Version: d.Version, Executor: d.Install, You: d.effective(id),
			UI: map[string]any{"auth": id.Auth}, Arenas: []arenaInfo{}}
		if d.Arenas != nil {
			if ns, err := d.Arenas(r.Context()); err == nil {
				sort.Strings(ns)
				for _, n := range ns {
					a := arenaInfo{Name: n, Excluded: []string{}}
					if d.Excluded != nil {
						if ex, err := d.Excluded(r.Context(), n); err == nil {
							a.Excluded = nonNil(ex)
						}
					}
					v.Arenas = append(v.Arenas, a)
				}
			}
		}
		if d.Autonomy != nil {
			s, src := d.Autonomy.Control.Get()
			v.Autonomous = &autonomousView{Settings: s, Source: src, Defaults: d.Autonomy.Control.Defaults()}
		}
		writeJSON(w, v)
	})

	mux.HandleFunc("POST /api/admin/autonomous", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayAdmin(w, r)
		if !ok {
			return
		}
		var s loop.Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&s); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		d.changeAutonomy(w, r, id, "configure", func(loop.Settings) loop.Settings { return s })
	})
	mux.HandleFunc("POST /api/admin/autonomous/reset", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayAdmin(w, r)
		if !ok {
			return
		}
		d.changeAutonomy(w, r, id, "reset", func(loop.Settings) loop.Settings { return d.Autonomy.Control.Defaults() })
	})
	mux.HandleFunc("POST /api/admin/pause", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayAdmin(w, r)
		if !ok {
			return
		}
		var req struct {
			Namespace string `json:"namespace"` // "" for all
			Paused    bool   `json:"paused"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		target := req.Namespace
		if target == "" {
			target = loop.PauseAll
		}
		action := "resume"
		if req.Paused {
			action = "pause"
		}
		d.changeAutonomy(w, r, id, action, func(cur loop.Settings) loop.Settings {
			cur.Paused = slices.DeleteFunc(cur.Paused, func(p string) bool { return p == target || (target == loop.PauseAll && !req.Paused) })
			if req.Paused {
				cur.Paused = append(cur.Paused, target)
			}
			return cur
		})
	})
	// halt is the emergency stop: pause autonomous mode everywhere first,
	// so the loop cannot start a fault in between, then clear every fault.
	// One request, so a slow or half-finished client cannot leave it done
	// by halves.
	mux.HandleFunc("POST /api/admin/halt", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayAdmin(w, r)
		if !ok {
			return
		}
		res := map[string]any{"paused": false, "cleared": []string{}, "failed": map[string]string{}}
		if d.Autonomy != nil {
			_, status, err := d.applyAutonomy(r.Context(), id, "halt", func(cur loop.Settings) loop.Settings {
				cur.Paused = []string{loop.PauseAll}
				return cur
			}, false)
			var kept keptError
			switch {
			case err == nil:
				res["paused"] = true
			case errors.As(err, &kept):
				res["paused"] = true
				res["warning"] = err.Error()
			default:
				http.Error(w, err.Error(), status)
				return
			}
		}
		if d.Active != nil && d.Executor != nil {
			faults, err := d.Active.ListActive(r.Context(), "")
			if err != nil {
				res["warning"] = "listing active faults: " + err.Error()
			}
			ctx := simian.WithActor(r.Context(), id.Email)
			cleared, failed := []string{}, map[string]string{}
			for _, f := range faults {
				if err := d.Executor.Clear(ctx, f.FaultUID); err != nil {
					failed[f.FaultUID] = err.Error()
					continue
				}
				cleared = append(cleared, f.FaultUID)
			}
			res["cleared"], res["failed"] = cleared, failed
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("POST /api/admin/clear-all", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayAdmin(w, r)
		if !ok {
			return
		}
		if d.Active == nil || d.Executor == nil {
			http.Error(w, "this controller does not clear faults from the web UI", http.StatusForbidden)
			return
		}
		var req struct {
			Namespace string `json:"namespace"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
		faults, err := d.Active.ListActive(r.Context(), req.Namespace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		ctx := simian.WithActor(r.Context(), id.Email)
		cleared, failed := []string{}, map[string]string{}
		for _, f := range faults {
			if err := d.Executor.Clear(ctx, f.FaultUID); err != nil {
				failed[f.FaultUID] = err.Error()
				continue
			}
			cleared = append(cleared, f.FaultUID)
		}
		writeJSON(w, map[string]any{"cleared": cleared, "failed": failed})
	})
}

// effective is id with the rights this controller can honour: writing needs
// an executor, administering needs autonomy controls or an executor.
func (d Deps) effective(id Identity) Identity {
	id.CanWrite = id.CanWrite && d.Executor != nil
	id.CanAdmin = id.CanAdmin && id.Auth == "iap" && (d.Autonomy != nil || d.Executor != nil)
	return id
}

// mayAdmin refuses the request unless the user may administer and it
// carries the UI's header.
func mayAdmin(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id := identityFrom(r.Context())
	switch {
	case id.Auth != "iap":
		http.Error(w, "the web UI administers autonomous mode only behind Identity-Aware Proxy", http.StatusForbidden)
	case !id.CanAdmin:
		http.Error(w, id.Email+" may not administer this controller (ui.iap.admins)", http.StatusForbidden)
	case r.Header.Get(writeHeader) == "":
		http.Error(w, "missing "+writeHeader+" header", http.StatusForbidden)
	default:
		return id, true
	}
	return id, false
}

// changeAutonomy applies edit to the current settings, checks the result
// against the install's limits, keeps it, and records who did what.
func (d Deps) changeAutonomy(w http.ResponseWriter, r *http.Request, id Identity, action string, edit func(loop.Settings) loop.Settings) {
	if d.Autonomy == nil {
		http.Error(w, "this controller does not take autonomous-mode settings from the web UI", http.StatusForbidden)
		return
	}
	view, status, err := d.applyAutonomy(r.Context(), id, action, edit, true)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, view)
}

// applyAutonomy is changeAutonomy without the HTTP. With validate false the
// limits are not checked — for a halt, which only ever pauses.
func (d Deps) applyAutonomy(ctx context.Context, id Identity, action string, edit func(loop.Settings) loop.Settings, validate bool) (autonomousView, int, error) {
	before, _ := d.Autonomy.Control.Get()
	after := edit(before)
	if validate && d.Limits != nil {
		lim, err := d.Limits(ctx)
		if err != nil {
			return autonomousView{}, http.StatusBadGateway, fmt.Errorf("reading the install's limits: %w", err)
		}
		if lim.Install == nil {
			inst := d.Autonomy.Control.Defaults()
			lim.Install = &inst
		}
		if err := after.Validate(lim); err != nil {
			return autonomousView{}, http.StatusUnprocessableEntity, err
		}
	}
	src := loop.Source{By: id.Email, At: time.Now().UTC()}
	if action == "reset" {
		src = loop.Source{}
	}
	var keepErr error
	if d.Autonomy.Store != nil {
		if action == "reset" {
			keepErr = d.Autonomy.Store.Clear(ctx)
		} else {
			keepErr = d.Autonomy.Store.Save(ctx, after, src)
		}
		if keepErr != nil && action != "halt" {
			// A halt takes effect regardless; anything else is not applied
			// if it could not be kept, so it cannot vanish at the next start.
			return autonomousView{}, http.StatusBadGateway, fmt.Errorf("keeping the settings: %w", keepErr)
		}
	}
	d.Autonomy.Control.Set(after, src)
	if d.Auditor != nil {
		d.Auditor.Emit(ctx, simian.AuditEvent{
			Event: audit.EventAutonomousConfigured, Reason: action,
			Payload: map[string]any{"actor": id.Email, "action": action, "before": before, "after": after},
		})
	}
	v := autonomousView{Settings: after, Source: src, Defaults: d.Autonomy.Control.Defaults()}
	if keepErr != nil {
		return v, http.StatusOK, keptError{keepErr}
	}
	return v, http.StatusOK, nil
}

// keptError is a halt that took effect but could not be kept across a
// restart.
type keptError struct{ err error }

func (k keptError) Error() string { return "keeping the halt across a restart: " + k.err.Error() }
