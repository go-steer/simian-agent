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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ProbeKind names the lifecycle slot a probe occupies.
type ProbeKind string

const (
	ProbeLiveness  ProbeKind = "liveness"
	ProbeReadiness ProbeKind = "readiness"
	ProbeStartup   ProbeKind = "startup"
)

// ProbeRewriterPort is where the simian-envoy-agent listens for the
// kubelet probe traffic redirected from each rewritten probe spec. This
// port MUST be added to the iptables exclude list at injection time —
// otherwise the agent's own listener gets PREROUTING-REDIRECTed to
// Envoy and the probe loop fails.
const ProbeRewriterPort = 15021

// ProbeRewriterPath is the URL path prefix the agent serves on
// ProbeRewriterPort. Format: /app-health/<container>/<liveness|readiness|startup>.
const ProbeRewriterPath = "/app-health"

// ProbeAnnotationPrefix is the pod-template-annotation key prefix the
// injector uses to stash the original probe specs and the agent reads
// at startup. Full key shape:
//
//	simian.chaos/probe-<containerName>-<probeKind>
//
// Value is the JSON-encoded StashedProbe. The kubelet probe itself is
// rewritten to httpGet{ path:/app-health/<container>/<kind>, port:15021 }.
const ProbeAnnotationPrefix = "simian.chaos/probe-"

// StashedProbe is the minimal subset of corev1.Probe needed to faithfully
// execute the original probe against 127.0.0.1 from inside the agent.
// We deliberately do NOT serialize the full corev1.Probe struct (timing
// fields like InitialDelaySeconds, PeriodSeconds, etc. stay on the
// rewritten probe spec on the kubelet side — they apply to the synthetic
// HTTP probe between kubelet and the agent, not between the agent and
// the workload).
type StashedProbe struct {
	// Exactly one of the following four blocks is set; matches the
	// oneof semantics of corev1.Probe.
	HTTPGet   *StashedHTTPGet   `json:"http_get,omitempty"`
	GRPC      *StashedGRPC      `json:"grpc,omitempty"`
	TCPSocket *StashedTCPSocket `json:"tcp_socket,omitempty"`
	// TimeoutSeconds caps the duration of any single probe attempt the
	// agent makes against the workload. Defaults to 1s if zero (matches
	// corev1.Probe's default).
	TimeoutSeconds int32 `json:"timeout_seconds,omitempty"`
}

// StashedHTTPGet mirrors corev1.HTTPGetAction. Always against 127.0.0.1.
type StashedHTTPGet struct {
	Path   string `json:"path,omitempty"`
	Port   int32  `json:"port"`
	Scheme string `json:"scheme,omitempty"` // "" or "HTTP" or "HTTPS"
	// Headers omitted in v1 — kubelet's HTTP probes rarely carry them.
}

// StashedGRPC mirrors corev1.GRPCAction. Always against 127.0.0.1.
type StashedGRPC struct {
	Port    int32  `json:"port"`
	Service string `json:"service,omitempty"`
}

// StashedTCPSocket mirrors corev1.TCPSocketAction. Always against 127.0.0.1.
type StashedTCPSocket struct {
	Port int32 `json:"port"`
}

// StashProbe captures the subset of corev1.Probe the agent will need to
// execute later. Returns nil + false if the probe is something we don't
// support (e.g. exec — would require a privileged exec into another
// container and is rare for kubelet probes; deferred).
func StashProbe(p *corev1.Probe) (StashedProbe, bool) {
	if p == nil {
		return StashedProbe{}, false
	}
	out := StashedProbe{TimeoutSeconds: p.TimeoutSeconds}
	switch {
	case p.HTTPGet != nil:
		out.HTTPGet = &StashedHTTPGet{
			Path:   p.HTTPGet.Path,
			Port:   p.HTTPGet.Port.IntVal,
			Scheme: string(p.HTTPGet.Scheme),
		}
		return out, true
	case p.GRPC != nil:
		out.GRPC = &StashedGRPC{Port: p.GRPC.Port}
		if p.GRPC.Service != nil {
			out.GRPC.Service = *p.GRPC.Service
		}
		return out, true
	case p.TCPSocket != nil:
		out.TCPSocket = &StashedTCPSocket{Port: p.TCPSocket.Port.IntVal}
		return out, true
	default:
		// Exec probes (or anything else) — not supported in v1.
		return StashedProbe{}, false
	}
}

// ProbeAnnotationKey returns the pod-template-annotation key for a
// container's probe of the given kind.
func ProbeAnnotationKey(containerName string, kind ProbeKind) string {
	return ProbeAnnotationPrefix + containerName + "-" + string(kind)
}

// ParseProbeAnnotationKey is the inverse of ProbeAnnotationKey. Returns
// the container name + probe kind if the key matches the expected
// shape, or empty strings if it doesn't. Used by the agent to enumerate
// supported probes from the downward-API annotations file.
func ParseProbeAnnotationKey(key string) (container string, kind ProbeKind) {
	if !strings.HasPrefix(key, ProbeAnnotationPrefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(key, ProbeAnnotationPrefix)
	// The kind is one of three known suffixes preceded by "-". Matching
	// against the suffix correctly handles container names that
	// themselves contain "-" (e.g. "redis-cart-liveness").
	for _, k := range []ProbeKind{ProbeLiveness, ProbeReadiness, ProbeStartup} {
		suffix := "-" + string(k)
		if strings.HasSuffix(rest, suffix) {
			return strings.TrimSuffix(rest, suffix), k
		}
	}
	return "", ""
}

// MarshalStashedProbe encodes a StashedProbe to the JSON shape the
// annotation carries. Stable across versions — be careful changing the
// field tags above; an injector writing a new shape with an old agent
// running will fail at execution time.
func MarshalStashedProbe(p StashedProbe) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal stashed probe: %w", err)
	}
	return string(b), nil
}

// UnmarshalStashedProbe is the inverse of MarshalStashedProbe.
func UnmarshalStashedProbe(s string) (StashedProbe, error) {
	var out StashedProbe
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return StashedProbe{}, fmt.Errorf("unmarshal stashed probe: %w", err)
	}
	return out, nil
}

// ExecuteProbe runs the stashed probe against 127.0.0.1 and returns
// nil on success, a non-nil error on failure. The agent translates a
// nil return to HTTP 200 and a non-nil return to HTTP 503.
//
// Probes always target loopback because the agent runs in the same pod
// (and therefore the same network namespace) as the workload container.
// Loopback traffic bypasses PREROUTING entirely, so the iptables
// REDIRECT to Envoy doesn't apply — the probe reaches the real workload
// port directly.
func ExecuteProbe(ctx context.Context, p StashedProbe) error {
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Second // matches corev1.Probe default
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch {
	case p.HTTPGet != nil:
		return executeHTTPGet(ctx, *p.HTTPGet)
	case p.GRPC != nil:
		return executeGRPC(ctx, *p.GRPC)
	case p.TCPSocket != nil:
		return executeTCPSocket(ctx, *p.TCPSocket)
	default:
		return fmt.Errorf("stashed probe is empty (no http_get / grpc / tcp_socket)")
	}
}

func executeHTTPGet(ctx context.Context, p StashedHTTPGet) error {
	scheme := strings.ToLower(p.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported HTTP scheme %q", p.Scheme)
	}
	path := p.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := scheme + "://127.0.0.1:" + strconv.Itoa(int(p.Port)) + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Match kubelet's HTTP-probe convention: any 2xx or 3xx is success.
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
}

func executeGRPC(ctx context.Context, p StashedGRPC) error {
	target := "127.0.0.1:" + strconv.Itoa(int(p.Port))
	// Insecure: kubelet's gRPC probes don't use TLS by default and the
	// workload is on loopback inside the same pod.
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc dial %s: %w", target, err)
	}
	defer func() { _ = conn.Close() }()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: p.Service})
	if err != nil {
		return fmt.Errorf("grpc health check %s: %w", target, err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("grpc health check %s: status %s", target, resp.Status)
	}
	return nil
}

func executeTCPSocket(ctx context.Context, p StashedTCPSocket) error {
	target := "127.0.0.1:" + strconv.Itoa(int(p.Port))
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("tcp dial %s: %w", target, err)
	}
	_ = conn.Close()
	return nil
}
