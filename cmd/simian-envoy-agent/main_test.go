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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/sut/envoy"
)

// TestParseAnnotationLine covers the downward-API rendering format
// the agent reads at startup. Format: key="quoted JSON value".
func TestParseAnnotationLine(t *testing.T) {
	cases := []struct {
		in        string
		wantKey   string
		wantValue string
		shouldOk  bool
	}{
		{
			in:        `simian.chaos/probe-server-liveness="{\"grpc\":{\"port\":5050}}"`,
			wantKey:   "simian.chaos/probe-server-liveness",
			wantValue: `{"grpc":{"port":5050}}`,
			shouldOk:  true,
		},
		{
			in:       `no.equals.here`,
			shouldOk: false,
		},
		{
			in:       `key=novalue_without_quotes`,
			shouldOk: false, // not double-quoted
		},
	}
	for _, tc := range cases {
		key, value, ok := parseAnnotationLine(tc.in)
		if ok != tc.shouldOk {
			t.Errorf("parseAnnotationLine(%q) ok=%v, want %v", tc.in, ok, tc.shouldOk)
			continue
		}
		if !ok {
			continue
		}
		if key != tc.wantKey || value != tc.wantValue {
			t.Errorf("parseAnnotationLine(%q) = (%q, %q), want (%q, %q)",
				tc.in, key, value, tc.wantKey, tc.wantValue)
		}
	}
}

func TestParseProbeURL(t *testing.T) {
	cases := []struct {
		path     string
		wantCont string
		wantKind envoy.ProbeKind
		ok       bool
	}{
		{"/app-health/server/liveness", "server", envoy.ProbeLiveness, true},
		{"/app-health/redis-cart/readiness", "redis-cart", envoy.ProbeReadiness, true},
		{"/app-health/server/startup", "server", envoy.ProbeStartup, true},
		{"/app-health/server/bogus", "", "", false},
		{"/other-path/server/liveness", "", "", false},
		{"/app-health/server", "", "", false}, // missing kind
	}
	for _, tc := range cases {
		cont, kind, ok := parseProbeURL(tc.path)
		if ok != tc.ok {
			t.Errorf("parseProbeURL(%q) ok=%v, want %v", tc.path, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if cont != tc.wantCont || kind != tc.wantKind {
			t.Errorf("parseProbeURL(%q) = (%q, %q), want (%q, %q)",
				tc.path, cont, kind, tc.wantCont, tc.wantKind)
		}
	}
}

// TestRegistryLoadsValidAnnotations writes a synthetic downward-API
// file, loads it, and confirms the right probes end up in the registry.
func TestRegistryLoadsValidAnnotations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "annotations")
	content := strings.Join([]string{
		`simian.chaos/probe-server-liveness="{\"grpc\":{\"port\":5050},\"timeout_seconds\":2}"`,
		`simian.chaos/probe-server-readiness="{\"http_get\":{\"path\":\"/_healthz\",\"port\":8080}}"`,
		`unrelated.annotation/foo="bar"`,
		`simian.chaos/probe-server-bogus="{\"grpc\":{\"port\":1234}}"`, // unknown kind suffix
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := newRegistry(path)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	if r.size() != 2 {
		t.Errorf("expected 2 probes (liveness + readiness, unrelated/bogus skipped); got %d", r.size())
	}
	if _, ok := r.get("server", envoy.ProbeLiveness); !ok {
		t.Error("server/liveness not in registry")
	}
	if _, ok := r.get("server", envoy.ProbeReadiness); !ok {
		t.Error("server/readiness not in registry")
	}
}

func TestRegistryMissingFileNotAnError(t *testing.T) {
	r, err := newRegistry("/nonexistent/path/annotations")
	if err != nil {
		t.Fatalf("missing file should not be an error; got %v", err)
	}
	if r.size() != 0 {
		t.Errorf("registry should be empty when file is missing; got %d entries", r.size())
	}
}

// TestHandleProbeReturns404WhenNoStashedProbe — the kubelet probe URL
// is well-formed but the agent has no matching entry. Surface as 404
// (loud signal that the injector and agent are out of sync) rather
// than silently 200 or 503.
func TestHandleProbeReturns404WhenMissing(t *testing.T) {
	reg, _ := newRegistry("/nonexistent")
	srv := httptest.NewServer(handleProbe(reg, slogTestLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + envoy.ProbeRewriterPath + "/server/liveness")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unmatched probe URL should 404; got %d", resp.StatusCode)
	}
}

// TestHandleProbeReturns200OnSuccess uses an httptest server as the
// "workload" and a stashed HTTP probe pointing at it.
func TestHandleProbeReturns200OnSuccess(t *testing.T) {
	// Workload server.
	workload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer workload.Close()
	port := portFromTestURL(t, workload.URL)

	// Synthetic registry with a single probe pointing at the workload.
	reg := &registry{probes: map[string]envoy.StashedProbe{
		"server/liveness": {HTTPGet: &envoy.StashedHTTPGet{Path: "/", Port: int32(port)}},
	}}
	// Override loopback target for this test by giving the stashed
	// probe a port the workload actually listens on; ExecuteProbe
	// dials 127.0.0.1:port which IS where httptest binds.
	srv := httptest.NewServer(handleProbe(reg, slogTestLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + envoy.ProbeRewriterPath + "/server/liveness")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("probe-rewrite path should return 200 on workload success; got %d", resp.StatusCode)
	}
}

// portFromTestURL extracts port from "http://127.0.0.1:34567".
func portFromTestURL(t *testing.T, url string) int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(url, "http://"), ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected URL: %s", url)
	}
	var p int
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			t.Fatalf("non-numeric port in %s", url)
		}
		p = p*10 + int(c-'0')
	}
	return p
}

// slogTestLogger returns a discard-target slog.Logger for tests so the
// agent's structured log lines don't clutter `go test` output.
func slogTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
