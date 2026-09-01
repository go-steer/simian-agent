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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// --- fakes ---

// stubLister returns a canned pod list, or an error.
type stubLister struct {
	mu       sync.Mutex
	pods     []Pod
	err      error
	sawNS    []string
	sawSel   []string
	callback func(call int) ([]Pod, error) // optional: vary the answer per call
	calls    int
}

func (l *stubLister) ListPods(_ context.Context, ns, sel string) ([]Pod, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.sawNS = append(l.sawNS, ns)
	l.sawSel = append(l.sawSel, sel)
	if l.callback != nil {
		return l.callback(l.calls)
	}
	return l.pods, l.err
}

// reply is one canned HTTP outcome.
type reply struct {
	status int
	body   string
	err    error
	// delay holds the response back before the headers are returned.
	delay time.Duration
	// bodyDelay holds the body back after the headers are returned, which is
	// how a netem delay on the return path actually shows up.
	bodyDelay time.Duration
}

// slowBody returns its content only after a pause, like a response whose
// headers arrived promptly and whose payload did not.
type slowBody struct {
	r     io.Reader
	delay time.Duration
	slept bool
}

func (b *slowBody) Read(p []byte) (int, error) {
	if !b.slept {
		b.slept = true
		time.Sleep(b.delay)
	}
	return b.r.Read(p)
}

func (b *slowBody) Close() error { return nil }

// stubDoer answers by URL. Anything unmatched is a connection refusal, which
// is what an unlisted endpoint would really do.
type stubDoer struct {
	mu       sync.Mutex
	byURL    map[string]reply
	fallback *reply
	seen     []string
	perCall  func(call int, url string) (reply, bool)
	calls    int
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	url := req.URL.String()
	d.seen = append(d.seen, url)
	r, ok := d.byURL[url]
	if d.perCall != nil {
		if pr, hit := d.perCall(call, url); hit {
			r, ok = pr, true
		}
	}
	if !ok {
		if d.fallback != nil {
			r, ok = *d.fallback, true
		}
	}
	d.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("dial tcp %s: connect: connection refused", req.URL.Host)
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	var body io.ReadCloser
	if r.bodyDelay > 0 {
		body = &slowBody{r: strings.NewReader(r.body), delay: r.bodyDelay}
	} else {
		body = io.NopCloser(strings.NewReader(r.body))
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       body,
		Request:    req,
	}, nil
}

func (d *stubDoer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// httpProbe wraps a spec map in a Settle-mode ProbeSpec.
func httpProbe(spec map[string]any) simian.ProbeSpec {
	return simian.ProbeSpec{
		Name: "reachability",
		Type: simian.ProbeTypeHTTP,
		Mode: simian.ProbeModeSettle,
		Spec: spec,
	}
}

// fastSpec adds short timing so a failing probe does not burn 90s.
func fastSpec(spec map[string]any) map[string]any {
	if _, ok := spec["timeout"]; !ok {
		spec["timeout"] = "150ms"
	}
	if _, ok := spec["interval"]; !ok {
		spec["interval"] = "10ms"
	}
	return spec
}

func webTarget() Target {
	return Target{Namespace: "boutique", Labels: map[string]string{"app": "web"}}
}

// --- spec validation ---

func TestHTTPProbeRejectsSpecsThatWouldPassUnconditionally(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want string
	}{
		{
			name: "no expectation at all",
			spec: map[string]any{"port": 8080},
			want: "states no condition",
		},
		{
			name: "empty expect_contains is not a condition",
			spec: map[string]any{"expect_contains": ""},
			want: "states no condition",
		},
		{
			name: "unreachable combined with a status it can never see",
			spec: map[string]any{"expect_unreachable": true, "expect_status": 503},
			want: "cannot be combined",
		},
		{
			name: "unreachable combined with a body it can never read",
			spec: map[string]any{"expect_unreachable": true, "expect_contains": "ok"},
			want: "cannot be combined",
		},
		{
			name: "latency window nothing can satisfy",
			spec: map[string]any{"min_latency": "500ms", "max_latency": "100ms"},
			want: "which nothing can satisfy",
		},
		{
			name: "status outside the HTTP range",
			spec: map[string]any{"expect_status": 42},
			want: "must be 100-599",
		},
		{
			name: "port outside the TCP range",
			spec: map[string]any{"expect_reachable": true, "port": 70000},
			want: "must be 1-65535",
		},
		{
			name: "fractional port",
			spec: map[string]any{"expect_reachable": true, "port": 80.5},
			want: "must be a whole number",
		},
		{
			name: "scheme we do not speak",
			spec: map[string]any{"expect_reachable": true, "scheme": "grpc"},
			want: "must be http or https",
		},
		{
			name: "timeout as a bare number is ambiguous",
			spec: map[string]any{"expect_reachable": true, "timeout": 30},
			want: "must be a duration string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewHTTPProber(&stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}, &stubDoer{})
			res := p.Run(context.Background(), httpProbe(tc.spec), webTarget())
			if res.Passed {
				t.Fatalf("probe passed; a spec that %s must be rejected", tc.name)
			}
			if res.Err == nil {
				t.Fatalf("Err is nil, want a rejection mentioning %q", tc.want)
			}
			if !strings.Contains(res.Err.Error(), tc.want) {
				t.Errorf("Err = %v, want it to mention %q", res.Err, tc.want)
			}
			if res.Attempts != 0 {
				t.Errorf("Attempts=%d, want 0 — a bad spec must fail before dialling", res.Attempts)
			}
		})
	}
}

func TestHTTPProbeNeedsSomethingToAimAt(t *testing.T) {
	p := NewHTTPProber(&stubLister{}, &stubDoer{})
	res := p.Run(context.Background(), httpProbe(map[string]any{"expect_reachable": true}), Target{Namespace: "boutique"})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "label_selector") {
		t.Fatalf("Err = %v, want a complaint about the missing selector", res.Err)
	}
}

func TestHTTPProbeNeedsANamespace(t *testing.T) {
	p := NewHTTPProber(&stubLister{}, &stubDoer{})
	res := p.Run(context.Background(), httpProbe(map[string]any{"expect_reachable": true}), Target{})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no namespace") {
		t.Fatalf("Err = %v, want a complaint about the missing namespace", res.Err)
	}
}

// --- reachability ---

func TestReachableIsAboutTheConnectionNotTheStatus(t *testing.T) {
	// A 500 is still a reachable workload. An SOT probe that demanded 200
	// would reject perfectly good faults against anything unhealthy.
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 500, body: "boom"}}}
	p := NewHTTPProber(lister, doer)

	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_reachable": true})), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}

	// And the other half of it: a refused connection is not reachable. This is
	// the SOT half of a partition gate, so passing here would let the Settle
	// half certify a workload that was already down before the fault.
	res = NewHTTPProber(lister, &stubDoer{}).
		Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_reachable": true})), webTarget())
	if res.Passed {
		t.Fatalf("probe passed against a refused connection: %s", res.Describe())
	}
	if !strings.Contains(res.Observed, "unreachable") {
		t.Errorf("Observed=%q, want it to record that the dial failed", res.Observed)
	}
}

func TestUnreachablePassesOnlyWhenTheConnectionActuallyFails(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	// Nothing registered: the stub refuses, like a real partitioned pod.
	p := NewHTTPProber(lister, &stubDoer{})
	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_unreachable": true})), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not pass against a refused connection: %s", res.Describe())
	}

	// Same probe, but the workload answers. This is the silent no-op the whole
	// mechanism exists to catch.
	answering := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok"}}}
	res = NewHTTPProber(lister, answering).
		Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_unreachable": true})), webTarget())
	if res.Passed {
		t.Fatal("probe passed against a workload that answered — a partition that did not partition would read as success")
	}
	if !strings.Contains(res.Observed, "200") {
		t.Errorf("Observed=%q, want the status that was actually seen", res.Observed)
	}
}

func TestEveryTargetPodMustSatisfyTheCondition(t *testing.T) {
	lister := &stubLister{pods: []Pod{
		{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}},
		{Name: "web-2", IP: "10.0.0.2", Ports: []int{8080}},
	}}
	// web-1 is cut off, web-2 is not. A partition that landed on one replica
	// of two is a partial no-op, and reporting it as applied would put a fault
	// in the eval record that half the traffic never saw.
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.2:8080/": {status: 200, body: "ok"}}}
	p := NewHTTPProber(lister, doer)

	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_unreachable": true})), webTarget())
	if res.Passed {
		t.Fatal("probe passed with one replica still answering")
	}
	if !strings.Contains(res.Observed, "web-2") {
		t.Errorf("Observed=%q, want the replica that broke the condition to be named", res.Observed)
	}
}

func TestNoPodsIsNotAPass(t *testing.T) {
	p := NewHTTPProber(&stubLister{pods: nil}, &stubDoer{})
	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_unreachable": true})), webTarget())
	if res.Passed {
		t.Fatal("probe passed with nothing to dial — an empty namespace is unreachable for reasons that have nothing to do with the fault")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no running pods") {
		t.Fatalf("Err = %v, want it to say there was nothing to probe", res.Err)
	}
}

// --- status, body, jsonpath ---

func TestExpectStatusComparesExactly(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 503, body: "fault filter abort"}}}
	p := NewHTTPProber(lister, doer)

	if res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_status": 503})), webTarget()); !res.Passed {
		t.Fatalf("503 probe did not pass: %s", res.Describe())
	}
	if res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_status": 500})), webTarget()); res.Passed {
		t.Fatal("probe expecting 500 passed against a 503")
	}
}

func TestJSONPathRendersTheBodyBeforeMatching(t *testing.T) {
	const runtimeJSON = `{"entries":{"fault.http.delay.fixed_delay_percent":{"final_value":"100","layer_values":["100"]}},"layers":["admin"]}`
	lister := &stubLister{pods: []Pod{{Name: "svc-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:15000/runtime": {status: 200, body: runtimeJSON}}}
	p := NewHTTPProber(lister, doer)

	spec := fastSpec(map[string]any{
		"port":          15000,
		"path":          "/runtime",
		"jsonpath":      `{.entries['fault\.http\.delay\.fixed_delay_percent'].final_value}`,
		"expect_equals": "100",
	})
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
	if res.Observed != "" && !strings.Contains(res.Observed, "100") {
		t.Errorf("Observed=%q, want the rendered value", res.Observed)
	}
}

func TestJSONPathSeesAnAbsentKeyAsEmptyRatherThanErroring(t *testing.T) {
	// A fresh Envoy reports {"entries":{}} — the fault key is simply not there.
	// The probe must read that as "not installed", not as a broken expression.
	lister := &stubLister{pods: []Pod{{Name: "svc-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{
		"http://10.0.0.1:15000/runtime": {status: 200, body: `{"entries":{},"layers":["admin"]}`},
	}}
	p := NewHTTPProber(lister, doer)

	spec := fastSpec(map[string]any{
		"port":          15000,
		"path":          "/runtime",
		"jsonpath":      `{.entries['fault\.http\.delay\.fixed_delay_percent'].final_value}`,
		"expect_equals": "100",
	})
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if res.Passed {
		t.Fatal("probe passed against an Envoy with no fault runtime installed")
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want a clean not-satisfied verdict rather than an error", res.Err)
	}
	// The distinction that matters: "the key is not there" is an observation
	// about the sidecar, not a complaint about the expression.
	if !strings.Contains(res.Observed, `value=""`) {
		t.Errorf("Observed=%q, want the absent key rendered as an empty value", res.Observed)
	}
}

func TestExpectEqualsDoesNotAcceptAPrefix(t *testing.T) {
	// "10" is a substring of "100". A percentage check that matched on
	// substrings would confirm a 10% fault against a 100% one.
	lister := &stubLister{pods: []Pod{{Name: "svc-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "100"}}}
	p := NewHTTPProber(lister, doer)

	if res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_equals": "10"})), webTarget()); res.Passed {
		t.Fatal(`expect_equals "10" passed against a body of "100"`)
	}
	if res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_contains": "10"})), webTarget()); !res.Passed {
		t.Fatal(`expect_contains "10" should still match "100" — that is what contains means`)
	}
}

func TestNonJSONBodyUnderAJSONPathIsAnObservationNotACrash(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "svc-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "<html>nope</html>"}}}
	p := NewHTTPProber(lister, doer)

	spec := fastSpec(map[string]any{"jsonpath": "{.x}", "expect_equals": "1"})
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if res.Passed {
		t.Fatal("probe passed against a non-JSON body")
	}
	if !strings.Contains(res.Observed, "not JSON") {
		t.Errorf("Observed=%q, want it to explain the body could not be parsed", res.Observed)
	}
}

// --- latency ---

func TestMinLatencyMeasuresTheRoundTrip(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	slow := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok", delay: 80 * time.Millisecond}}}
	p := NewHTTPProber(lister, slow)

	spec := map[string]any{"min_latency": "50ms", "timeout": "1s", "interval": "10ms", "request_timeout": "2s"}
	if res := p.Run(context.Background(), httpProbe(spec), webTarget()); !res.Passed {
		t.Fatalf("slow response did not satisfy min_latency: %s", res.Describe())
	}

	fast := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok"}}}
	spec = map[string]any{"min_latency": "50ms", "timeout": "150ms", "interval": "10ms", "request_timeout": "2s"}
	if res := NewHTTPProber(lister, fast).Run(context.Background(), httpProbe(spec), webTarget()); res.Passed {
		t.Fatal("a delay probe passed against a workload that answered immediately — the fault injected nothing")
	}
}

func TestLatencyCoversTheBodyNotJustTheHeaders(t *testing.T) {
	// netem delays the packets, not the response line. An envoy fixed-delay
	// behaves the same way. A clock that stops when the headers land would
	// measure ~0 and report that the delay never landed.
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{
		"http://10.0.0.1:8080/": {status: 200, body: "ok", bodyDelay: 80 * time.Millisecond},
	}}
	p := NewHTTPProber(lister, doer)

	spec := map[string]any{"min_latency": "50ms", "timeout": "1s", "interval": "10ms", "request_timeout": "2s"}
	if res := p.Run(context.Background(), httpProbe(spec), webTarget()); !res.Passed {
		t.Fatalf("a response whose body was held back for 80ms did not satisfy min_latency=50ms: %s", res.Describe())
	}
}

func TestMaxLatencyRejectsAnAlreadySlowBaseline(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	slow := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok", delay: 80 * time.Millisecond}}}
	p := NewHTTPProber(lister, slow)

	spec := map[string]any{"expect_reachable": true, "max_latency": "20ms", "timeout": "200ms", "interval": "10ms", "request_timeout": "2s"}
	if res := p.Run(context.Background(), httpProbe(spec), webTarget()); res.Passed {
		t.Fatal("SOT probe passed against a baseline already slower than the fault would make it")
	}
}

// --- ports, targeting, retries ---

func TestPortIsInferredFromTheContainerPortAndOverriddenBySpec(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{9376, 8080}}}}
	doer := &stubDoer{byURL: map[string]reply{
		"http://10.0.0.1:9376/": {status: 200, body: "inferred"},
		"http://10.0.0.1:8080/": {status: 200, body: "explicit"},
	}}
	p := NewHTTPProber(lister, doer)

	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_contains": "inferred"})), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not use the first container port: %s", res.Describe())
	}
	res = p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"port": 8080, "expect_contains": "explicit"})), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not use the port the spec named: %s", res.Describe())
	}
}

func TestAPodWithNoPortIsAnErrorNotAnEndlessRetry(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1"}}}
	p := NewHTTPProber(lister, &stubDoer{})
	res := p.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_reachable": true})), webTarget())
	if res.Passed {
		t.Fatal("probe passed against a pod it could not dial")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "declares no container port") {
		t.Fatalf("Err = %v, want it to name the missing port", res.Err)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts=%d, want 1 — a spec that can never work should not be retried", res.Attempts)
	}
}

func TestTheProbeInheritsTheFaultsNamespaceAndLabels(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok"}}}
	target := Target{Namespace: "boutique", Labels: map[string]string{"app": "web", "tier": "front"}}

	res := NewHTTPProber(lister, doer).
		Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_reachable": true})), target)
	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
	if lister.sawNS[0] != "boutique" {
		t.Errorf("namespace=%q, want the fault's own", lister.sawNS[0])
	}
	// Sorted, so the selector is stable regardless of map iteration order.
	if lister.sawSel[0] != "app=web,tier=front" {
		t.Errorf("selector=%q, want the fault's own labels", lister.sawSel[0])
	}
}

func TestAnExplicitSelectorBeatsTheInheritedOne(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "db-1", IP: "10.0.0.9", Ports: []int{5432}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.9:5432/": {status: 200, body: "ok"}}}
	spec := fastSpec(map[string]any{"expect_reachable": true, "label_selector": "app=db"})

	NewHTTPProber(lister, doer).Run(context.Background(), httpProbe(spec), webTarget())
	if lister.sawSel[0] != "app=db" {
		t.Errorf("selector=%q, want the one the spec named", lister.sawSel[0])
	}
}

func TestNamingAPodProbesOnlyThatPod(t *testing.T) {
	// The label selector reaches the whole replica set; `name` narrows it to
	// one. A probe that quietly dialled the siblings too would fail on a fault
	// that was only ever applied to the pod it named.
	lister := &stubLister{pods: []Pod{
		{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}},
		{Name: "web-2", IP: "10.0.0.2", Ports: []int{8080}},
	}}
	// Only web-1 is cut off.
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.2:8080/": {status: 200, body: "ok"}}}
	p := NewHTTPProber(lister, doer)

	spec := fastSpec(map[string]any{"expect_unreachable": true, "name": "web-1"})
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if !res.Passed {
		t.Fatalf("probe did not pass against the pod it named: %s", res.Describe())
	}
	if strings.Contains(res.Observed, "web-2") {
		t.Errorf("Observed=%q, want only the named pod to have been dialled", res.Observed)
	}

	// And a name that matches nothing is not a pass either.
	res = NewHTTPProber(lister, doer).
		Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_unreachable": true, "name": "web-9"})), webTarget())
	if res.Passed {
		t.Fatal("probe passed against a pod name that matches nothing")
	}
}

func TestTheProbeKeepsPollingUntilTheFaultLands(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	// Answers for the first two calls, then the partition takes hold.
	doer := &stubDoer{perCall: func(call int, _ string) (reply, bool) {
		if call <= 2 {
			return reply{status: 200, body: "ok"}, true
		}
		return reply{}, false
	}}
	p := NewHTTPProber(lister, doer)

	spec := map[string]any{"expect_unreachable": true, "timeout": "2s", "interval": "10ms"}
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if !res.Passed {
		t.Fatalf("probe gave up before the fault took hold: %s", res.Describe())
	}
	if res.Attempts < 3 {
		t.Errorf("Attempts=%d, want at least 3 — the probe should have retried", res.Attempts)
	}
}

func TestTheTargetSetIsResolvedEveryPollNotOnce(t *testing.T) {
	// A fault's targets churn: a partition can be applied while a replica is
	// still starting, and a probe holding the pod list it saw on its first poll
	// would go on certifying a set of pods that no longer exists.
	lister := &stubLister{callback: func(call int) ([]Pod, error) {
		if call < 3 {
			return []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}, nil
		}
		// web-2 rolls in and is still answering.
		return []Pod{
			{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}},
			{Name: "web-2", IP: "10.0.0.2", Ports: []int{8080}},
		}, nil
	}}
	// Both answer, so the probe keeps polling and the later list is reached.
	doer := &stubDoer{byURL: map[string]reply{
		"http://10.0.0.1:8080/": {status: 200, body: "ok"},
		"http://10.0.0.2:8080/": {status: 200, body: "ok"},
	}}
	p := NewHTTPProber(lister, doer)

	spec := map[string]any{"expect_unreachable": true, "timeout": "150ms", "interval": "10ms"}
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if res.Passed {
		t.Fatalf("probe passed on a stale pod list — web-2 was never dialled: %s", res.Describe())
	}
	if !strings.Contains(res.Observed, "web-2") {
		t.Errorf("Observed=%q, want the replica that appeared mid-probe", res.Observed)
	}
}

func TestAListingErrorIsRetriedThenReported(t *testing.T) {
	lister := &stubLister{err: errors.New("etcdserver: request timed out")}
	p := NewHTTPProber(lister, &stubDoer{})
	spec := map[string]any{"expect_reachable": true, "timeout": "60ms", "interval": "10ms"}
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if res.Passed {
		t.Fatal("probe passed while it could not even list pods")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "etcdserver") {
		t.Fatalf("Err = %v, want the underlying listing failure", res.Err)
	}
	if lister.calls < 2 {
		t.Errorf("calls=%d, want the probe to have retried a transient listing error", lister.calls)
	}
}

func TestCancellingTheContextStopsTheProbe(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok"}}}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewHTTPProber(lister, doer)

	done := make(chan Result, 1)
	go func() {
		done <- p.Run(ctx, httpProbe(map[string]any{"expect_unreachable": true, "timeout": "30s", "interval": "10ms"}), webTarget())
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Passed {
			t.Fatal("probe passed after cancellation")
		}
		if res.Err == nil || !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("Err = %v, want context.Canceled", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not return after its context was cancelled")
	}
}

func TestRequestTimeoutBoundsASingleDial(t *testing.T) {
	// A dropped packet hangs rather than refusing. The per-request deadline is
	// what turns that hang into an observation inside one poll.
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok", delay: time.Hour}}}
	p := NewHTTPProber(lister, doer)

	start := time.Now()
	spec := map[string]any{"expect_unreachable": true, "timeout": "2s", "interval": "10ms", "request_timeout": "50ms"}
	res := p.Run(context.Background(), httpProbe(spec), webTarget())
	if !res.Passed {
		t.Fatalf("a hung dial did not read as unreachable: %s", res.Describe())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %s; the per-request deadline should have cut the first dial short", elapsed)
	}
	if doer.callCount() == 0 {
		t.Error("no request was made")
	}
}

// --- production pod lister ---

func TestEveryAttemptDialsAFreshConnection(t *testing.T) {
	// Found in a cluster, not in a unit test: a NetworkPolicy partition landed,
	// conntrack kept the socket the SOT probe had opened, and the Settle probe
	// went on reading 200s in 1ms through it — reporting a working fault as a
	// no-op. Nothing about a reachability probe is meaningful over a pooled
	// connection.
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	var reused []bool
	doer := &recordingDoer{stubDoer: stubDoer{byURL: map[string]reply{
		"http://10.0.0.1:8080/": {status: 200, body: "ok"},
	}}, onRequest: func(r *http.Request) { reused = append(reused, !r.Close) }}
	p := NewHTTPProber(lister, doer)

	spec := map[string]any{"expect_unreachable": true, "timeout": "60ms", "interval": "10ms"}
	p.Run(context.Background(), httpProbe(spec), webTarget())
	if len(reused) == 0 {
		t.Fatal("no requests were made")
	}
	for i, r := range reused {
		if r {
			t.Fatalf("request %d left the connection poolable; every poll must dial", i)
		}
	}
}

// recordingDoer is a stubDoer that reports each request before answering it.
type recordingDoer struct {
	stubDoer
	onRequest func(*http.Request)
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.onRequest(req)
	return d.stubDoer.Do(req)
}

// theProductionClientDoesNotPoolConnections is asserted here rather than in a
// live test: NewKubernetesHTTPProber's client is the one thing req.Close alone
// would not cover if a future change swapped the transport.
func TestTheProductionProberDisablesKeepAlives(t *testing.T) {
	p := NewKubernetesHTTPProber(k8sfake.NewSimpleClientset())
	c, ok := p.http.(*http.Client)
	if !ok {
		t.Fatalf("prober client is %T, want *http.Client", p.http)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Error("the production prober pools connections — a partition would read as a no-op")
	}
}

func TestKubernetesPodListerSkipsWhatCannotBeDialled(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		listerPod("running-with-ip", corev1.PodRunning, "10.0.0.1", 8080),
		listerPod("running-no-ip", corev1.PodRunning, "", 8080),
		listerPod("pending", corev1.PodPending, "10.0.0.2", 8080),
	)
	l := &KubernetesPodLister{Clientset: cs}
	pods, err := l.ListPods(context.Background(), "boutique", "app=web")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "running-with-ip" {
		t.Fatalf("pods = %+v, want only the running pod with an IP", pods)
	}
	if len(pods[0].Ports) != 1 || pods[0].Ports[0] != 8080 {
		t.Errorf("ports = %v, want the declared container port", pods[0].Ports)
	}
}

func listerPod(name string, phase corev1.PodPhase, ip string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "boutique",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Ports: []corev1.ContainerPort{{ContainerPort: port}},
			}},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

// --- mux ---

func TestMuxRoutesByTypeAndRefusesUnknownOnes(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}
	doer := &stubDoer{byURL: map[string]reply{"http://10.0.0.1:8080/": {status: 200, body: "ok"}}}
	mux := NewMux(map[string]Prober{simian.ProbeTypeHTTP: NewHTTPProber(lister, doer)})

	if res := mux.Run(context.Background(), httpProbe(fastSpec(map[string]any{"expect_reachable": true})), webTarget()); !res.Passed {
		t.Fatalf("http probe did not route: %s", res.Describe())
	}

	cmd := simian.ProbeSpec{Name: "shell-out", Type: simian.ProbeTypeCmd, Mode: simian.ProbeModeSettle}
	res := mux.Run(context.Background(), cmd, webTarget())
	if res.Passed {
		t.Fatal("a probe type nothing can run was treated as passing")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no prober for type") {
		t.Fatalf("Err = %v, want it to name the missing prober", res.Err)
	}
}
