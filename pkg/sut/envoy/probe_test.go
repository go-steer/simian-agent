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

package envoy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestStashProbeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *corev1.Probe
	}{
		{
			name: "http_get",
			in: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/_healthz", Port: intstr.FromInt(8080), Scheme: corev1.URISchemeHTTP,
					},
				},
				TimeoutSeconds: 2,
			},
		},
		{
			name: "grpc",
			in: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					GRPC: &corev1.GRPCAction{Port: 5050},
				},
			},
		},
		{
			name: "tcp_socket",
			in: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(6379)},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ok := StashProbe(tc.in)
			if !ok {
				t.Fatalf("StashProbe should support %s", tc.name)
			}
			encoded, err := MarshalStashedProbe(s)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			decoded, err := UnmarshalStashedProbe(encoded)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Spot-check a representative field per shape.
			switch tc.name {
			case "http_get":
				if decoded.HTTPGet == nil || decoded.HTTPGet.Port != 8080 {
					t.Errorf("http_get port lost in round-trip: %+v", decoded)
				}
			case "grpc":
				if decoded.GRPC == nil || decoded.GRPC.Port != 5050 {
					t.Errorf("grpc port lost in round-trip: %+v", decoded)
				}
			case "tcp_socket":
				if decoded.TCPSocket == nil || decoded.TCPSocket.Port != 6379 {
					t.Errorf("tcp_socket port lost in round-trip: %+v", decoded)
				}
			}
		})
	}
}

func TestStashProbeRejectsExec(t *testing.T) {
	p := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"true"}},
		},
	}
	if _, ok := StashProbe(p); ok {
		t.Error("StashProbe should NOT support exec probes (v1 limitation)")
	}
}

func TestStashProbeNilReturnsFalse(t *testing.T) {
	if _, ok := StashProbe(nil); ok {
		t.Error("nil probe should return ok=false")
	}
}

func TestParseProbeAnnotationKey(t *testing.T) {
	cases := []struct {
		key         string
		wantCont    string
		wantKind    ProbeKind
		shouldMatch bool
	}{
		{"simian.chaos/probe-server-liveness", "server", ProbeLiveness, true},
		{"simian.chaos/probe-server-readiness", "server", ProbeReadiness, true},
		{"simian.chaos/probe-server-startup", "server", ProbeStartup, true},
		// Container names with hyphens are common in K8s; ensure they
		// don't get split on the wrong "-".
		{"simian.chaos/probe-redis-cart-liveness", "redis-cart", ProbeLiveness, true},
		{"simian.chaos/probe-server-bogus", "", "", false},
		{"unrelated.annotation/foo", "", "", false},
	}
	for _, tc := range cases {
		cont, kind := ParseProbeAnnotationKey(tc.key)
		if tc.shouldMatch {
			if cont != tc.wantCont || kind != tc.wantKind {
				t.Errorf("Parse(%q) = (%q, %q), want (%q, %q)",
					tc.key, cont, kind, tc.wantCont, tc.wantKind)
			}
		} else {
			if cont != "" {
				t.Errorf("Parse(%q) should have rejected; got container=%q", tc.key, cont)
			}
		}
	}
}

// TestExecuteHTTPGet against an httptest server confirms the agent
// path: response 2xx → success, 4xx/5xx → error.
func TestExecuteHTTPGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/redir":
			w.WriteHeader(301)
		case "/down":
			w.WriteHeader(503)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	port := portFromTestURL(t, srv.URL)
	for _, tc := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"200", "/ok", false},
		{"301 redirect counts as success", "/redir", false},
		{"503", "/down", true},
		{"404", "/missing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := executeHTTPGet(context.Background(), StashedHTTPGet{Path: tc.path, Port: int32(port)})
			if tc.wantErr && err == nil {
				t.Errorf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want success, got %v", err)
			}
		})
	}
}

// TestExecuteTCPSocket dials a real listener and a known-closed port.
func TestExecuteTCPSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	t.Run("open port succeeds", func(t *testing.T) {
		if err := executeTCPSocket(context.Background(), StashedTCPSocket{Port: int32(port)}); err != nil {
			t.Errorf("open port: %v", err)
		}
	})
	t.Run("closed port fails", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		// Use a port we just closed so this is reliably refused.
		ln.Close()
		if err := executeTCPSocket(ctx, StashedTCPSocket{Port: int32(port)}); err == nil {
			t.Error("closed port should fail")
		}
	})
}

func TestExecuteProbeEmptyFails(t *testing.T) {
	if err := ExecuteProbe(context.Background(), StashedProbe{}); err == nil {
		t.Error("empty probe should fail (no action set)")
	}
}

func TestProbeAnnotationKey(t *testing.T) {
	got := ProbeAnnotationKey("frontend", ProbeReadiness)
	want := "simian.chaos/probe-frontend-readiness"
	if got != want {
		t.Errorf("ProbeAnnotationKey = %q, want %q", got, want)
	}
}

// portFromTestURL extracts the port from an httptest URL like
// "http://127.0.0.1:34567".
func portFromTestURL(t *testing.T, url string) int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(url, "http://"), ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected test URL shape: %s", url)
	}
	p, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return p
}
