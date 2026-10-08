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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// writeHeader must be on every write. A page on another site can make the
// browser send IAP's cookie, but not a custom header without a CORS
// preflight this server never answers — so a cross-site request cannot
// inject a fault in the user's name.
const writeHeader = "X-Simian-UI"

// submitRequest is the submit form. Mode "manifest" names the fault
// exactly, as `simian chaos --kind --spec` does; mode "intent" asks the LLM
// to translate Intent, as `simian chaos --intent` does.
type submitRequest struct {
	Mode      string         `json:"mode"`
	Namespace string         `json:"namespace"`
	Workload  string         `json:"workload"`
	Engine    string         `json:"engine"`
	Kind      string         `json:"kind"`
	Spec      map[string]any `json:"spec"`
	Duration  string         `json:"duration"`
	Intent    string         `json:"intent"`
}

func writeRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		id := identityFrom(r.Context())
		id.CanWrite = id.CanWrite && d.Executor != nil
		writeJSON(w, id)
	})
	mux.HandleFunc("GET /api/catalog", func(w http.ResponseWriter, r *http.Request) {
		if d.Catalog == nil {
			writeJSON(w, []simian.CatalogEntry{})
			return
		}
		cat, err := d.Catalog(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for i := range cat {
			cat[i].SchemaJSON = nil // large, and the form does not use it
		}
		writeJSON(w, nonNil(cat))
	})
	mux.HandleFunc("POST /api/faults", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayWrite(w, r, d)
		if !ok {
			return
		}
		var req submitRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		ctx := simian.WithActor(r.Context(), id.Email)
		m, err := manifestFor(r, d, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.Source = simian.SourceDirected
		uid, err := d.Executor.Apply(ctx, m)
		if err != nil {
			http.Error(w, err.Error(), statusFor(err))
			return
		}
		writeJSON(w, map[string]string{"fault_uid": uid, "engine": string(m.Engine), "kind": m.ResourceKind})
	})
	mux.HandleFunc("POST /api/faults/{uid}/clear", func(w http.ResponseWriter, r *http.Request) {
		id, ok := mayWrite(w, r, d)
		if !ok {
			return
		}
		if err := d.Executor.Clear(simian.WithActor(r.Context(), id.Email), r.PathValue("uid")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]bool{"cleared": true})
	})
}

// mayWrite refuses the request unless writes are on, the user may write,
// and the request carries the UI's header.
func mayWrite(w http.ResponseWriter, r *http.Request, d Deps) (Identity, bool) {
	id := identityFrom(r.Context())
	switch {
	case d.Executor == nil:
		http.Error(w, "this controller does not accept faults from the web UI", http.StatusForbidden)
	case id.Auth != "iap":
		http.Error(w, "the web UI submits and clears faults only behind Identity-Aware Proxy; use the simian CLI here", http.StatusForbidden)
	case !id.CanWrite:
		http.Error(w, id.Email+" may view but not submit or clear faults (ui.iap.writers)", http.StatusForbidden)
	case r.Header.Get(writeHeader) == "":
		http.Error(w, "missing "+writeHeader+" header", http.StatusForbidden)
	default:
		return id, true
	}
	return id, false
}

func manifestFor(r *http.Request, d Deps, req submitRequest) (simian.FaultManifest, error) {
	if req.Namespace == "" {
		return simian.FaultManifest{}, errors.New("namespace is required")
	}
	dur := 2 * time.Minute
	if req.Duration != "" {
		var err error
		if dur, err = time.ParseDuration(req.Duration); err != nil {
			return simian.FaultManifest{}, fmt.Errorf("duration: %w", err)
		}
	}
	switch req.Mode {
	case "intent":
		if d.Translate == nil {
			return simian.FaultManifest{}, errors.New("this controller has no LLM to translate a request")
		}
		if req.Intent == "" {
			return simian.FaultManifest{}, errors.New("intent is required")
		}
		return d.Translate(r.Context(), req.Intent, req.Namespace, dur)
	case "manifest", "":
		if req.Kind == "" {
			return simian.FaultManifest{}, errors.New("kind is required")
		}
		entry, err := catalogEntry(r, d, req.Engine, req.Kind)
		if err != nil {
			return simian.FaultManifest{}, err
		}
		target := simian.TargetRef{Namespace: req.Namespace, Name: req.Workload}
		return simian.FaultManifest{
			Engine: entry.Engine, APIVersion: entry.APIVersion, ResourceKind: entry.ResourceKind,
			Spec: req.Spec, Targets: []simian.TargetRef{target}, Duration: dur,
		}, nil
	default:
		return simian.FaultManifest{}, fmt.Errorf("mode %q: want manifest or intent", req.Mode)
	}
}

// catalogEntry finds kind in the catalog, so the page need not know each
// engine's apiVersion.
func catalogEntry(r *http.Request, d Deps, engine, kind string) (simian.CatalogEntry, error) {
	if d.Catalog == nil {
		return simian.CatalogEntry{}, errors.New("no catalog")
	}
	cat, err := d.Catalog(r.Context())
	if err != nil {
		return simian.CatalogEntry{}, err
	}
	for _, c := range cat {
		if c.ResourceKind == kind && (engine == "" || string(c.Engine) == engine) {
			return c, nil
		}
	}
	return simian.CatalogEntry{}, fmt.Errorf("kind %q is not in the catalog", kind)
}

// statusFor maps the executor's refusals to 422 — the request was
// understood and the safety checks said no — and anything else to 502.
func statusFor(err error) int {
	var ee *simian.ExecutorError
	if errors.As(err, &ee) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}
