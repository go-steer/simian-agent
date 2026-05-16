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

// simian-envoy-agent is a small sidecar binary deployed alongside the
// Envoy fault sidecar (pkg/sut/envoy). It exists to solve the
// kubelet-gRPC-probe-vs-Envoy interaction documented at
// https://go-steer.github.io/simian-agent/docs/known-limitations/
//
// Flow:
//  1. The injector (pkg/sut/envoy.Inject) rewrites each container's
//     liveness/readiness/startup probe to an httpGet against this agent
//     on ProbeRewriterPort. The original probe spec is stashed in a
//     pod-template annotation (simian.chaos/probe-<container>-<kind>).
//  2. The agent reads its pod's annotations from a downward-API volume
//     at /etc/simian-envoy-agent/annotations on startup.
//  3. Kubelet probes the rewritten httpGet URL. The iptables init
//     container has been told to exempt ProbeRewriterPort, so the
//     request bypasses Envoy and reaches this agent.
//  4. The agent dispatches /app-health/<container>/<kind> to the
//     matching stashed probe, executes it against 127.0.0.1 (loopback
//     bypasses PREROUTING entirely, so it reaches the real workload
//     port not Envoy), and returns 200 or 503.
//
// Configuration: ANNOTATIONS_FILE env var overrides the default
// downward-API mount path. LISTEN_ADDR env var overrides the default
// listen address (":15021").
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-steer/simian-agent/pkg/sut/envoy"
)

const (
	defaultListenAddr     = ":15021"
	defaultAnnotationFile = "/etc/simian-envoy-agent/annotations"
)

func main() {
	os.Exit(run())
}

// run is the testable entry point. Returns the desired process exit
// code; main() just calls os.Exit(run()). Pattern keeps every defer
// in run() correctly scoped (gocritic exitAfterDefer would fire if
// os.Exit and the deferred signal cancel were both in main()).
func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = defaultListenAddr
	}
	annotationsPath := os.Getenv("ANNOTATIONS_FILE")
	if annotationsPath == "" {
		annotationsPath = defaultAnnotationFile
	}

	registry, err := newRegistry(annotationsPath)
	if err != nil {
		logger.Error("registry init failed", slog.String("err", err.Error()))
		return 1
	}
	logger.Info("simian-envoy-agent: probe registry loaded",
		slog.Int("probe_count", registry.size()),
		slog.String("annotations_path", annotationsPath))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Self-health. Always 200 — the agent has no upstream dep.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(envoy.ProbeRewriterPath+"/", handleProbe(registry, logger))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("simian-envoy-agent: listening", slog.String("addr", addr))
		errCh <- srv.ListenAndServe()
	}()

	return waitForShutdown(ctx, srv, errCh, logger)
}

// waitForShutdown blocks until either the shutdown signal arrives or
// the server exits on its own. Pulled out of main() so the deferred
// shutdownCancel() actually fires before os.Exit (a defer in main()
// after os.Exit silently skips — gocritic exitAfterDefer).
func waitForShutdown(ctx context.Context, srv *http.Server, errCh <-chan error, logger *slog.Logger) int {
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server exited", slog.String("err", err.Error()))
			return 1
		}
		return 0
	}
}

// handleProbe is the per-request handler. URL shape:
// /app-health/<container>/<kind> where kind ∈ {liveness, readiness, startup}.
// Returns 200 on probe success, 503 on probe failure, 404 if no
// matching stashed probe was found (kubelet should never hit this if
// the injector wired things correctly — surface loudly).
func handleProbe(reg *registry, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		container, kind, ok := parseProbeURL(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		probe, ok := reg.get(container, kind)
		if !ok {
			logger.Warn("no stashed probe for request",
				slog.String("container", container),
				slog.String("kind", string(kind)))
			http.NotFound(w, r)
			return
		}
		if err := envoy.ExecuteProbe(r.Context(), probe); err != nil {
			logger.Info("probe failed",
				slog.String("container", container),
				slog.String("kind", string(kind)),
				slog.String("err", err.Error()))
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// parseProbeURL extracts the container + kind from a /app-health/...
// URL path. Returns false if the path is malformed.
func parseProbeURL(path string) (container string, kind envoy.ProbeKind, ok bool) {
	rest := strings.TrimPrefix(path, envoy.ProbeRewriterPath+"/")
	if rest == path {
		return "", "", false // prefix not present
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	switch envoy.ProbeKind(parts[1]) {
	case envoy.ProbeLiveness, envoy.ProbeReadiness, envoy.ProbeStartup:
		return parts[0], envoy.ProbeKind(parts[1]), true
	}
	return "", "", false
}

// registry is the in-memory map of (container,kind) → StashedProbe
// populated at agent startup from the downward-API annotations file.
//
// The file format is the standard downward-API rendering of pod
// annotations: one line per annotation, "key=value" where the value is
// a quoted string. Example:
//
//	simian.chaos/probe-server-liveness="{\"grpc\":{\"port\":5050}}"
//	simian.chaos/probe-server-readiness="{\"http_get\":{\"path\":\"/_healthz\",\"port\":8080}}"
type registry struct {
	mu     sync.RWMutex
	probes map[string]envoy.StashedProbe // key = "<container>/<kind>"
}

func newRegistry(path string) (*registry, error) {
	r := &registry{probes: map[string]envoy.StashedProbe{}}
	if err := r.load(path); err != nil {
		return nil, err
	}
	return r, nil
}

// load reads the downward-API annotations file and populates the
// registry. Lines that don't parse, don't match the probe-annotation
// prefix, or carry a value the agent can't decode are skipped with a
// warning — a single bad annotation shouldn't stop the agent from
// serving the others.
//
// Missing file is NOT an error: it can mean the pod has no probes
// to rewrite, in which case the agent should still come up to satisfy
// its own readiness probe.
func (r *registry) load(path string) error {
	// G703 false positive: `path` is configured by the operator via the
	// ANNOTATIONS_FILE env var (or the baked-in default). It's not
	// user-controlled in any meaningful sense; the operator could just
	// mount whatever filesystem they want into the pod regardless.
	// #nosec G703 -- operator-controlled path.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("annotations file not present; agent will serve no probes",
				slog.String("path", path))
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := parseAnnotationLine(line)
		if !ok {
			slog.Warn("skip malformed annotation line", slog.String("line", line))
			continue
		}
		container, kind := envoy.ParseProbeAnnotationKey(key)
		if container == "" {
			// Not one of our probe annotations — ignore silently.
			continue
		}
		probe, err := envoy.UnmarshalStashedProbe(value)
		if err != nil {
			slog.Warn("skip un-decodable probe annotation",
				slog.String("key", key),
				slog.String("err", err.Error()))
			continue
		}
		r.probes[container+"/"+string(kind)] = probe
	}
	return nil
}

func (r *registry) get(container string, kind envoy.ProbeKind) (envoy.StashedProbe, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.probes[container+"/"+string(kind)]
	return p, ok
}

func (r *registry) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.probes)
}

// parseAnnotationLine parses one line of the downward-API annotations
// rendering. Format: `key="quoted JSON value with escapes"`.
//
// The downward API quotes the value and escapes inner quotes; we use
// strconv.Unquote to invert that.
func parseAnnotationLine(line string) (key, value string, ok bool) {
	eq := strings.Index(line, "=")
	if eq <= 0 {
		return "", "", false
	}
	key = line[:eq]
	rawValue := line[eq+1:]
	// Downward-API values are double-quoted with the standard escape
	// sequences. strconv.Unquote handles "..." with the right semantics.
	unquoted, err := unquoteAnnotation(rawValue)
	if err != nil {
		return "", "", false
	}
	return key, unquoted, true
}

// unquoteAnnotation strips the surrounding double-quotes that the
// downward API renders around the value, and processes the standard
// escape sequences. Pulled out of parseAnnotationLine so it can be
// unit-tested directly.
func unquoteAnnotation(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", fmt.Errorf("value not double-quoted")
	}
	// Use the strconv path — handles \", \\, \n, etc.
	out, err := strconv.Unquote(raw)
	if err != nil {
		return "", err
	}
	return out, nil
}
