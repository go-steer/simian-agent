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
	"time"

	"github.com/oklog/ulid/v2"

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
		writeJSON(w, d.effective(identityFrom(r.Context())))
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
		// The UID is known before the executor runs, so the page can follow
		// the fault on the event stream even if this request answers first.
		m.UID = "f-" + ulid.Make().String()
		applied := make(chan error, 1)
		// Detached from the request: a fault being applied is finished, or
		// rolled back by the executor, whether or not the browser waits.
		go func() {
			_, err := d.Executor.Apply(context.WithoutCancel(ctx), m)
			applied <- err
		}()
		res := map[string]string{"fault_uid": m.UID, "engine": string(m.Engine), "kind": m.ResourceKind}
		select {
		case err := <-applied:
			if err != nil {
				http.Error(w, err.Error(), statusFor(err))
				return
			}
			res["state"] = "applied"
			writeJSON(w, res)
		case <-time.After(applyWait):
			// Settling — waiting for the engine and the probes — can outlast
			// a load balancer's request timeout. Answer now; the outcome
			// arrives on /api/events and in /api/faults under this UID.
			res["state"] = "applying"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(res)
		}
	})
	mux.HandleFunc("POST /api/translate", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := mayWrite(w, r, d); !ok {
			return
		}
		var req submitRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.Mode = "intent"
		m, err := manifestFor(r, d, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, proposalOf(m))
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

// applyWait is how long a submit waits for the executor before answering
// 202 and leaving the outcome to the event stream: short enough to stay
// well inside a load balancer's request timeout, long enough that a
// refusal — which comes before the driver — is answered directly.
var applyWait = 15 * time.Second

// proposal is a translated fault, for the operator to confirm, edit or
// discard before anything is applied. It has the fields the submit form
// takes, so confirming it is an exact submit.
type proposal struct {
	Engine    string         `json:"engine"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace"`
	Workload  string         `json:"workload"`
	Spec      map[string]any `json:"spec"`
	Duration  string         `json:"duration"`
	Rationale string         `json:"rationale,omitempty"`
}

func proposalOf(m simian.FaultManifest) proposal {
	p := proposal{Engine: string(m.Engine), Kind: m.ResourceKind, Spec: m.Spec, Duration: m.Duration.String(), Rationale: m.Rationale}
	if len(m.Targets) > 0 {
		p.Namespace, p.Workload = m.Targets[0].Namespace, m.Targets[0].Name
	}
	return p
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
