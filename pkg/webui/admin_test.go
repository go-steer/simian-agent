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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/simian"
)

type memStore struct {
	saved   *loop.Settings
	by      string
	cleared bool
}

func (m *memStore) Save(_ context.Context, s loop.Settings, src loop.Source) error {
	m.saved, m.by, m.cleared = &s, src.By, false
	return nil
}
func (m *memStore) Clear(context.Context) error { m.saved, m.cleared = nil, true; return nil }

type events struct {
	mu sync.Mutex
	e  []simian.AuditEvent
}

func (a *events) Emit(_ context.Context, e simian.AuditEvent) {
	a.mu.Lock()
	a.e = append(a.e, e)
	a.mu.Unlock()
}

type activeList struct{ faults []simian.ActiveFault }

func (a activeList) ListActive(context.Context, string) ([]simian.ActiveFault, error) {
	return a.faults, nil
}

type adminWorld struct {
	srv     *httptest.Server
	iap     *fakeIAP
	control *loop.Control
	store   *memStore
	audit   *events
	exec    *fakeExecutor
}

func newAdminWorld(t *testing.T) *adminWorld {
	t.Helper()
	f := newFakeIAP(t)
	w := &adminWorld{iap: f, store: &memStore{}, audit: &events{}, exec: &fakeExecutor{},
		control: loop.NewControl(loop.Settings{Namespaces: []string{"boutique"}, Interval: 10 * time.Minute, MaxFaultsPerCycle: 1, MaxSeverityPerCycle: simian.TierNamespace})}
	auth := f.auth(t, testAudience, "alice@example.com", "bob@example.com")
	auth.Admins = Writers{"alice@example.com"}
	w.srv = httptest.NewServer(Handler(Deps{
		Version: "v-test", Auth: auth, Executor: w.exec, Auditor: w.audit,
		Active: activeList{faults: []simian.ActiveFault{{FaultUID: "f-a"}, {FaultUID: "f-b"}}},
		Arenas: func(context.Context) ([]string, error) { return []string{"bank", "boutique"}, nil },
		Excluded: func(_ context.Context, ns string) ([]string, error) {
			if ns == "boutique" {
				return []string{"loadgenerator"}, nil
			}
			return nil, nil
		},
		Install:  Install{MaxConcurrentFaults: 1, DurationCeiling: "5m0s", PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}},
		Autonomy: &Autonomy{Control: w.control, Store: w.store},
		Limits: func(context.Context) (loop.Limits, error) {
			return loop.Limits{MaxConcurrentFaults: 1, PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}, Arenas: []string{"bank", "boutique"}}, nil
		},
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *adminWorld) as(t *testing.T, email string) string {
	return w.iap.token(t, "ES256", func(c map[string]any) { c["email"] = email })
}

func TestTheConfigurationPanelShowsWhatTheControllerRunsWith(t *testing.T) {
	w := newAdminWorld(t)
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/config", nil)
	req.Header.Set(iapHeader, w.as(t, "carol@example.com"))
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v configView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Executor.MaxConcurrentFaults != 1 || len(v.Arenas) != 2 || v.Arenas[1].Name != "boutique" ||
		len(v.Arenas[1].Excluded) != 1 || v.Autonomous == nil || v.Autonomous.Settings.Interval != 10*time.Minute ||
		v.You.Email != "carol@example.com" || v.You.CanAdmin || v.You.CanWrite {
		t.Errorf("config = %+v", v)
	}
}

func TestAnAdminConfiguresAutonomousModeWithinTheLimits(t *testing.T) {
	w := newAdminWorld(t)
	on := `{"enabled":true,"namespaces":["boutique","bank"],"interval":"3m","max_faults_per_cycle":1,"max_severity_per_cycle":"namespace","hypothesis":"payments"}`

	if code, body := post(t, w.srv, "/api/admin/autonomous", w.as(t, "bob@example.com"), true, on); code != http.StatusForbidden {
		t.Errorf("a writer who is not an admin: %d %s", code, body)
	}
	if code, body := post(t, w.srv, "/api/admin/autonomous", w.as(t, "alice@example.com"), false, on); code != http.StatusForbidden {
		t.Errorf("no X-Simian-UI header: %d %s", code, body)
	}
	over := strings.Replace(on, `"max_faults_per_cycle":1`, `"max_faults_per_cycle":3`, 1)
	if code, body := post(t, w.srv, "/api/admin/autonomous", w.as(t, "alice@example.com"), true, over); code != http.StatusUnprocessableEntity || !strings.Contains(body, "maxConcurrentFaults") {
		t.Errorf("over the executor's limit: %d %s", code, body)
	}
	if s, _ := w.control.Get(); s.Enabled {
		t.Fatal("a refused change was applied")
	}

	if code, body := post(t, w.srv, "/api/admin/autonomous", w.as(t, "alice@example.com"), true, on); code != 200 {
		t.Fatalf("configure: %d %s", code, body)
	}
	s, src := w.control.Get()
	if !s.Enabled || s.Interval != 3*time.Minute || len(s.Namespaces) != 2 || s.Hypothesis != "payments" || src.By != "alice@example.com" {
		t.Errorf("control = %+v from %+v", s, src)
	}
	if w.store.saved == nil || !w.store.saved.Enabled || w.store.by != "alice@example.com" {
		t.Errorf("not kept for a restart: %+v", w.store)
	}

	if code, body := post(t, w.srv, "/api/admin/pause", w.as(t, "alice@example.com"), true, `{"namespace":"bank","paused":true}`); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if s, _ := w.control.Get(); !s.IsPaused("bank") || s.IsPaused("boutique") {
		t.Errorf("after pausing bank: %+v", s.Paused)
	}
	if code, _ := post(t, w.srv, "/api/admin/pause", w.as(t, "alice@example.com"), true, `{"paused":false}`); code != 200 {
		t.Fatal("resume all")
	}
	if s, _ := w.control.Get(); len(s.Paused) != 0 {
		t.Errorf("after resuming all: %+v", s.Paused)
	}

	if code, _ := post(t, w.srv, "/api/admin/autonomous/reset", w.as(t, "alice@example.com"), true, ""); code != 200 {
		t.Fatal("reset")
	}
	if s, src := w.control.Get(); s.Enabled || s.Interval != 10*time.Minute || src.By != "" || !w.store.cleared {
		t.Errorf("after reset: %+v from %+v, store cleared %v", s, src, w.store.cleared)
	}

	var actions []string
	for _, e := range w.audit.e {
		if e.Event == audit.EventAutonomousConfigured {
			if e.Payload["actor"] != "alice@example.com" || e.Payload["before"] == nil || e.Payload["after"] == nil {
				t.Errorf("event without actor, before or after: %+v", e)
			}
			actions = append(actions, e.Reason)
		}
	}
	if strings.Join(actions, ",") != "configure,pause,resume,reset" {
		t.Errorf("audited actions = %v", actions)
	}
}

func TestAnAdminClearsEveryFaultInTheirName(t *testing.T) {
	w := newAdminWorld(t)
	if code, _ := post(t, w.srv, "/api/admin/clear-all", w.as(t, "bob@example.com"), true, `{}`); code != http.StatusForbidden {
		t.Errorf("a writer who is not an admin cleared all: %d", code)
	}
	code, body := post(t, w.srv, "/api/admin/clear-all", w.as(t, "alice@example.com"), true, `{}`)
	if code != 200 || !strings.Contains(body, `"f-a"`) || !strings.Contains(body, `"f-b"`) {
		t.Fatalf("clear-all: %d %s", code, body)
	}
	if len(w.exec.cleared) != 2 || w.exec.actors[0] != "alice@example.com" || w.exec.actors[1] != "alice@example.com" {
		t.Errorf("cleared %v as %v", w.exec.cleared, w.exec.actors)
	}
}
