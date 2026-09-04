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

package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/jsonpath"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Default per-request timeout for an http probe. Short on purpose: a dropped
// packet has to register as a failure well inside one poll interval, or an
// "unreachable" probe spends its whole budget on a single hung dial.
const DefaultRequestTimeout = 3 * time.Second

// bodyLimit caps how much of a response we read. Probe bodies are status
// pages and admin JSON; anything larger is not being matched on.
const bodyLimit = 64 * 1024

// Pod is the slice of a Kubernetes pod an http probe needs.
type Pod struct {
	Name string
	IP   string
	// Ports are the declared container ports, in podspec order. Used when the
	// probe spec does not name one.
	Ports []int
}

// PodLister resolves the pods an http probe should dial.
type PodLister interface {
	ListPods(ctx context.Context, namespace, labelSelector string) ([]Pod, error)
}

// Doer is the minimal HTTP surface the prober needs. Real callers pass an
// *http.Client; tests pass a stub.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPProber implements the "http" probe type: dial the fault's target pods
// directly and assert something about what comes back — or about the fact that
// nothing does.
//
// This is the probe type dataplane faults need. A partition, a delay, or an
// injected 503 leaves no field on any Kubernetes object to read: the only
// honest proof is a request that behaves differently than it did a moment ago.
// The controller dials pod IPs directly, the same way the envoy-fault driver
// reaches each sidecar's admin API.
type HTTPProber struct {
	pods PodLister
	http Doer
}

// NewHTTPProber constructs a prober over the given pod lister and HTTP client.
func NewHTTPProber(pods PodLister, doer Doer) *HTTPProber {
	return &HTTPProber{pods: pods, http: doer}
}

// NewKubernetesHTTPProber is the production wiring: resolve pods with the
// clientset, dial them with a client that never pools a connection. No
// client-level timeout — each request carries its own deadline from the probe
// spec.
//
// Keep-alive is off deliberately, and it is not a micro-optimisation in
// reverse. A NetworkPolicy or a netem partition drops *new* flows; conntrack
// lets an already-established one through. A probe that reused the socket its
// SOT check opened would go on getting 200s in 1ms through a partition that
// really did land, and reject a working fault. Every poll must dial.
func NewKubernetesHTTPProber(cs kubernetes.Interface) *HTTPProber {
	return NewHTTPProber(&KubernetesPodLister{Clientset: cs}, &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
	})
}

// KubernetesPodLister resolves pods through the API server.
type KubernetesPodLister struct {
	Clientset kubernetes.Interface
}

// ListPods implements PodLister, returning only pods that are running with an
// assigned IP — the rest cannot be dialled, and counting them would make a
// reachability probe fail for a reason that has nothing to do with the fault.
func (l *KubernetesPodLister) ListPods(ctx context.Context, namespace, labelSelector string) ([]Pod, error) {
	if l.Clientset == nil {
		return nil, fmt.Errorf("http probe: pod lister has no clientset")
	}
	list, err := l.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s (selector=%q): %w", namespace, labelSelector, err)
	}
	out := make([]Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		var ports []int
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				ports = append(ports, int(cp.ContainerPort))
			}
		}
		out = append(out, Pod{Name: p.Name, IP: p.Status.PodIP, Ports: ports})
	}
	return out, nil
}

// httpSpec is the decoded ProbeSpec.Spec for an http probe.
type httpSpec struct {
	namespace     string
	labelSelector string
	podName       string
	port          int
	path          string
	scheme        string
	method        string
	jsonPath      string

	expectStatus      int
	expectContains    string
	expectEquals      string
	expectReachable   bool
	expectUnreachable bool
	minLatency        time.Duration
	maxLatency        time.Duration

	timeout        time.Duration
	interval       time.Duration
	requestTimeout time.Duration
}

// describe renders the success condition for a timeout message.
func (s httpSpec) describe() string {
	var parts []string
	if s.expectUnreachable {
		parts = append(parts, "connection to fail")
	}
	if s.expectReachable {
		parts = append(parts, "connection to succeed")
	}
	if s.expectStatus != 0 {
		parts = append(parts, fmt.Sprintf("status %d", s.expectStatus))
	}
	if s.expectContains != "" {
		parts = append(parts, fmt.Sprintf("%q in %s", s.expectContains, s.valueName()))
	}
	if s.expectEquals != "" {
		parts = append(parts, fmt.Sprintf("%s == %q", s.valueName(), s.expectEquals))
	}
	if s.minLatency > 0 {
		part := fmt.Sprintf("latency >= %s", s.minLatency)
		if s.slowCountsAsTimeout() {
			part += fmt.Sprintf(" (or no response within %s)", s.requestTimeout)
		}
		parts = append(parts, part)
	}
	if s.maxLatency > 0 {
		parts = append(parts, fmt.Sprintf("latency <= %s", s.maxLatency))
	}
	return strings.Join(parts, " and ") + ", on every target pod"
}

func (s httpSpec) valueName() string {
	if s.jsonPath != "" {
		return fmt.Sprintf("jsonpath %s", s.jsonPath)
	}
	return "body"
}

// attempt is one request against one pod.
type attempt struct {
	pod      Pod
	endpoint string
	status   int
	value    string // body, or the jsonpath rendering of it
	latency  time.Duration
	err      error // transport-level failure: no response at all
	fatal    error // the probe cannot run against this pod (no port, bad jsonpath)
}

// describe renders one attempt for the Observed field.
func (a attempt) describe() string {
	switch {
	case a.fatal != nil:
		return fmt.Sprintf("%s: %v", a.pod.Name, a.fatal)
	case a.err != nil:
		return fmt.Sprintf("%s (%s): unreachable after %s: %v",
			a.pod.Name, a.endpoint, a.latency.Round(time.Millisecond), a.err)
	default:
		return fmt.Sprintf("%s (%s): %d in %s, value=%q",
			a.pod.Name, a.endpoint, a.status, a.latency.Round(time.Millisecond),
			truncate(a.value, 120))
	}
}

// satisfied reports whether one attempt meets every stated expectation. Only
// reached for attempts that ran: Run returns on the first fatal before any of
// this is consulted.
func (s httpSpec) satisfied(a attempt) bool {
	if s.expectUnreachable {
		return a.err != nil
	}
	if a.err != nil {
		return s.timedOutSlowly(a)
	}
	if s.expectStatus != 0 && a.status != s.expectStatus {
		return false
	}
	if s.expectContains != "" && !strings.Contains(a.value, s.expectContains) {
		return false
	}
	if s.expectEquals != "" && strings.TrimSpace(a.value) != s.expectEquals {
		return false
	}
	if s.minLatency > 0 && a.latency < s.minLatency {
		return false
	}
	if s.maxLatency > 0 && a.latency > s.maxLatency {
		return false
	}
	return true
}

// slowCountsAsTimeout reports whether this spec is one where a request that
// never came back is itself the evidence being looked for.
//
// Only a pure min_latency probe qualifies. A response that never arrived has
// no status, no body and no jsonpath to check, so a spec asserting any of
// those cannot be satisfied by its absence; and the wait has to be at least as
// long as the threshold or the timeout proves nothing about it.
func (s httpSpec) slowCountsAsTimeout() bool {
	return s.minLatency > 0 &&
		!s.expectReachable &&
		s.expectStatus == 0 &&
		s.expectContains == "" &&
		s.expectEquals == "" &&
		s.requestTimeout >= s.minLatency
}

// timedOutSlowly reports whether a failed attempt still answers the question
// the probe asked.
//
// A min_latency gate asks "is the target answering slower than X". A request
// that has not come back after X has answered it: whatever the target is
// doing, it is not doing it in under X. Counting that as a failure is how a
// netem delay that landed hard enough to blow the request timeout gets rolled
// back and reported as a fault that did nothing — the mirror image of the
// vacuous pass this gate exists to prevent, and worse, because the fault was
// live in the cluster for the whole settle window before being disowned.
//
// Measured on GKE Dataplane V2: a 250ms delay on Online Boutique's frontend
// took the page from ~90ms to ~3.9s, because the request fans out into a
// dozen internal round trips and every one of them pays the delay twice. No
// per-request timeout derived from the injected latency alone survives that.
//
// Only timeouts count. A refused connection comes back immediately and says
// the target is down, which is not slowness. The SOT half of the gate is what
// makes the inference safe either way: the target was proven reachable and
// fast seconds earlier, so "not answering now" is a change rather than a
// pre-existing condition.
func (s httpSpec) timedOutSlowly(a attempt) bool {
	return s.slowCountsAsTimeout() && a.latency >= s.minLatency && isTimeout(a.err)
}

// isTimeout distinguishes "waited and got nothing" from "asked and was
// refused". http.Client wraps the request context's deadline in a *url.Error,
// which reports Timeout() and unwraps to context.DeadlineExceeded; both forms
// are checked so a change in either layer does not quietly turn a timeout into
// a refusal.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Run implements Prober.
func (h *HTTPProber) Run(ctx context.Context, p simian.ProbeSpec, target Target) Result {
	res := Result{Name: p.Name, Type: p.Type}
	spec, err := parseHTTPSpec(p.Spec, target)
	if err != nil {
		res.Err = fmt.Errorf("probe %q: %w", p.Name, err)
		return res
	}
	res.Expected = spec.describe()

	var jp *jsonpath.JSONPath
	if spec.jsonPath != "" {
		jp = jsonpath.New(p.Name)
		jp.AllowMissingKeys(true)
		if err := jp.Parse(spec.jsonPath); err != nil {
			res.Err = fmt.Errorf("probe %q: parse jsonpath %q: %w", p.Name, spec.jsonPath, err)
			return res
		}
	}

	start := time.Now()
	deadline := start.Add(spec.timeout)
	var lastErr error
	for {
		res.Attempts++
		attempts, err := h.round(ctx, spec, jp)
		if err != nil {
			// No pods yet, or the API server said no. Both are worth retrying —
			// a fault's targets can be mid-restart — but remember the reason so
			// a probe that only ever errored reports it.
			lastErr = err
			res.Observed = ""
		} else {
			lastErr = nil
			res.Observed = describeAttempts(attempts)
			if fatal := firstFatal(attempts); fatal != nil {
				res.Err = fmt.Errorf("probe %q: %w", p.Name, fatal)
				res.Elapsed = time.Since(start)
				return res
			}
			if allSatisfied(spec, attempts) {
				res.Passed = true
				res.Elapsed = time.Since(start)
				return res
			}
		}

		if time.Now().After(deadline) {
			res.Elapsed = time.Since(start)
			if lastErr != nil {
				res.Err = fmt.Errorf("probe %q: %w", p.Name, lastErr)
			}
			return res
		}
		select {
		case <-time.After(spec.interval):
		case <-ctx.Done():
			res.Elapsed = time.Since(start)
			res.Err = fmt.Errorf("probe %q: %w", p.Name, ctx.Err())
			return res
		}
	}
}

// round resolves the target pods and hits each one once.
func (h *HTTPProber) round(ctx context.Context, spec httpSpec, jp *jsonpath.JSONPath) ([]attempt, error) {
	pods, err := h.pods.ListPods(ctx, spec.namespace, spec.labelSelector)
	if err != nil {
		return nil, err
	}
	if spec.podName != "" {
		filtered := pods[:0:0]
		for _, p := range pods {
			if p.Name == spec.podName {
				filtered = append(filtered, p)
			}
		}
		pods = filtered
	}
	if len(pods) == 0 {
		// Not a pass. "Nothing to dial" is the vacuous success this package
		// exists to refuse: an unreachability probe would otherwise be
		// satisfied by a namespace with no workload in it at all.
		return nil, fmt.Errorf("no running pods with an IP match namespace=%q selector=%q name=%q",
			spec.namespace, spec.labelSelector, spec.podName)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	out := make([]attempt, 0, len(pods))
	for _, pod := range pods {
		out = append(out, h.hit(ctx, spec, jp, pod))
	}
	return out, nil
}

// hit performs one request against one pod.
func (h *HTTPProber) hit(ctx context.Context, spec httpSpec, jp *jsonpath.JSONPath, pod Pod) attempt {
	a := attempt{pod: pod}
	port := spec.port
	if port == 0 {
		if len(pod.Ports) == 0 {
			a.fatal = fmt.Errorf("pod %q declares no container port, and the probe does not set %q", pod.Name, "port")
			return a
		}
		port = pod.Ports[0]
	}
	a.endpoint = fmt.Sprintf("%s://%s:%d%s", spec.scheme, pod.IP, port, spec.path)

	reqCtx, cancel := context.WithTimeout(ctx, spec.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, spec.method, a.endpoint, nil)
	if err != nil {
		a.fatal = fmt.Errorf("build request for %s: %w", a.endpoint, err)
		return a
	}
	// Force a fresh dial per attempt, whatever Doer we were handed. A pooled
	// connection survives a partition — conntrack keeps established flows —
	// so reusing one would report a fault that landed as one that did not.
	req.Close = true

	start := time.Now()
	resp, err := h.http.Do(req)
	a.latency = time.Since(start)
	if err != nil {
		a.err = err
		return a
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	// Time the read too: a delay fault can hold the body rather than the
	// headers, and stopping the clock at the first byte would miss it.
	a.latency = time.Since(start)
	a.status = resp.StatusCode
	if readErr != nil {
		a.err = fmt.Errorf("read body from %s: %w", a.endpoint, readErr)
		return a
	}
	a.value = string(body)
	if jp != nil {
		rendered, err := renderJSONPath(jp, body)
		if err != nil {
			a.err = fmt.Errorf("%s: %w", a.endpoint, err)
			return a
		}
		a.value = rendered
	}
	return a
}

// renderJSONPath parses the body as JSON and evaluates the expression over it.
func renderJSONPath(jp *jsonpath.JSONPath, body []byte) (string, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("response body is not JSON: %w", err)
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, doc); err != nil {
		return "", fmt.Errorf("evaluate jsonpath: %w", err)
	}
	return buf.String(), nil
}

func allSatisfied(spec httpSpec, attempts []attempt) bool {
	for _, a := range attempts {
		if !spec.satisfied(a) {
			return false
		}
	}
	// Unreachable today — round refuses a zero-pod result before we get here —
	// and kept anyway. Every other guard in this file fails closed if it is
	// wrong; this one is the difference between "no pods" and "verified", so
	// it is the one place a second check is worth an untestable branch.
	return len(attempts) > 0
}

func firstFatal(attempts []attempt) error {
	for _, a := range attempts {
		if a.fatal != nil {
			return a.fatal
		}
	}
	return nil
}

func describeAttempts(attempts []attempt) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, a.describe())
	}
	return strings.Join(parts, "; ")
}

// parseHTTPSpec decodes and validates an http probe's Spec map.
func parseHTTPSpec(raw map[string]any, target Target) (httpSpec, error) {
	s := httpSpec{
		path:           "/",
		scheme:         "http",
		method:         http.MethodGet,
		timeout:        DefaultTimeout,
		interval:       DefaultInterval,
		requestTimeout: DefaultRequestTimeout,
	}
	var err error
	if s.namespace, err = optString(raw, "namespace"); err != nil {
		return s, err
	}
	if s.labelSelector, err = optString(raw, "label_selector"); err != nil {
		return s, err
	}
	if s.podName, err = optString(raw, "name"); err != nil {
		return s, err
	}
	if s.port, err = optInt(raw, "port"); err != nil {
		return s, err
	}
	if v, err := optString(raw, "path"); err != nil {
		return s, err
	} else if v != "" {
		s.path = v
	}
	if v, err := optString(raw, "scheme"); err != nil {
		return s, err
	} else if v != "" {
		s.scheme = v
	}
	if v, err := optString(raw, "method"); err != nil {
		return s, err
	} else if v != "" {
		s.method = strings.ToUpper(v)
	}
	if s.jsonPath, err = optString(raw, "jsonpath"); err != nil {
		return s, err
	}
	if s.expectStatus, err = optInt(raw, "expect_status"); err != nil {
		return s, err
	}
	if s.expectContains, err = optString(raw, "expect_contains"); err != nil {
		return s, err
	}
	if s.expectEquals, err = optString(raw, "expect_equals"); err != nil {
		return s, err
	}
	if s.expectReachable, err = optBool(raw, "expect_reachable"); err != nil {
		return s, err
	}
	if s.expectUnreachable, err = optBool(raw, "expect_unreachable"); err != nil {
		return s, err
	}
	if s.minLatency, err = optDuration(raw, "min_latency", 0); err != nil {
		return s, err
	}
	if s.maxLatency, err = optDuration(raw, "max_latency", 0); err != nil {
		return s, err
	}
	if s.timeout, err = optDuration(raw, "timeout", s.timeout); err != nil {
		return s, err
	}
	if s.interval, err = optDuration(raw, "interval", s.interval); err != nil {
		return s, err
	}
	if s.requestTimeout, err = optDuration(raw, "request_timeout", s.requestTimeout); err != nil {
		return s, err
	}

	if s.namespace == "" {
		s.namespace = target.Namespace
	}
	if s.namespace == "" {
		return s, fmt.Errorf("http probe: no namespace, and the fault declares no target namespace to fall back to")
	}
	if s.labelSelector == "" && s.podName == "" {
		s.labelSelector = target.Selector()
	}
	if s.labelSelector == "" && s.podName == "" {
		return s, fmt.Errorf("http probe: needs %q or %q, and the fault declares no target labels to fall back to",
			"label_selector", "name")
	}
	if !strings.HasPrefix(s.path, "/") {
		s.path = "/" + s.path
	}
	if s.scheme != "http" && s.scheme != "https" {
		return s, fmt.Errorf("http probe: %q must be http or https, got %q", "scheme", s.scheme)
	}
	if s.port < 0 || s.port > 65535 {
		return s, fmt.Errorf("http probe: %q must be 1-65535, got %d", "port", s.port)
	}
	if s.expectStatus != 0 && (s.expectStatus < 100 || s.expectStatus > 599) {
		return s, fmt.Errorf("http probe: %q must be 100-599, got %d", "expect_status", s.expectStatus)
	}

	// Same rule as the k8s probe: a probe that states no condition is not a
	// check, it is a check-shaped no-op. "expect_contains": "" is not a
	// condition either — strings.Contains(anything, "") is always true.
	stated := 0
	for _, on := range []bool{
		s.expectUnreachable, s.expectReachable,
		s.expectStatus != 0, s.expectContains != "", s.expectEquals != "",
		s.minLatency > 0, s.maxLatency > 0,
	} {
		if on {
			stated++
		}
	}
	if stated == 0 {
		return s, fmt.Errorf("http probe: states no condition; set one of %s: a probe with none passes unconditionally",
			"expect_unreachable, expect_reachable, expect_status, expect_contains, expect_equals, min_latency, max_latency")
	}
	if s.expectUnreachable && stated > 1 {
		return s, fmt.Errorf("http probe: %q cannot be combined with any other expectation — there is no response to assert on",
			"expect_unreachable")
	}
	if s.minLatency > 0 && s.maxLatency > 0 && s.minLatency > s.maxLatency {
		return s, fmt.Errorf("http probe: %q (%s) is above %q (%s), which nothing can satisfy",
			"min_latency", s.minLatency, "max_latency", s.maxLatency)
	}
	if s.timeout <= 0 {
		return s, fmt.Errorf("http probe: %q must be positive, got %s", "timeout", s.timeout)
	}
	if s.interval <= 0 {
		return s, fmt.Errorf("http probe: %q must be positive, got %s", "interval", s.interval)
	}
	if s.requestTimeout <= 0 {
		return s, fmt.Errorf("http probe: %q must be positive, got %s", "request_timeout", s.requestTimeout)
	}
	return s, nil
}
