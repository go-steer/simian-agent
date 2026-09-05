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
	"fmt"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// fakeLogs is a LogSource over a fixed map of pod name to log text. A name
// mapped to an error returns it instead.
type fakeLogs struct {
	mu   sync.Mutex
	pods []string
	logs map[string]string
	errs map[string]error

	// listErr fails the pod listing itself.
	listErr error

	// seen records the options each read was made with, so a test can assert
	// the spec reached the API call rather than only the parser.
	seen []LogOptions

	// onRead runs before each read, letting a test change the world between
	// polls.
	onRead func(round int)
	rounds int
}

func (f *fakeLogs) PodNames(context.Context, string, string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pods, nil
}

func (f *fakeLogs) PodLog(_ context.Context, _, pod string, opts LogOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onRead != nil {
		f.rounds++
		f.onRead(f.rounds)
	}
	f.seen = append(f.seen, opts)
	if err, ok := f.errs[pod]; ok {
		return "", err
	}
	return f.logs[pod], nil
}

func logsProbe(spec map[string]any) simian.ProbeSpec {
	return simian.ProbeSpec{Name: "stalled", Type: simian.ProbeTypeLogs, Mode: simian.ProbeModeSettle, Spec: spec}
}

// logsTarget is what a fault hands its probes: the namespace it aimed at and
// the labels it aimed with.
var logsTarget = Target{Namespace: "arena-1", Labels: map[string]string{"app": "checkout"}}

func TestALogsProbePassesWhenAnyPodSaysIt(t *testing.T) {
	src := &fakeLogs{
		pods: []string{"checkout-a", "checkout-b"},
		logs: map[string]string{
			"checkout-a": "starting up\nlistening on :8080\n",
			"checkout-b": "starting up\nlevel=error msg=\"upstream request failed\" upstream=payments\n",
		},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1s",
	}), logsTarget)

	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
	if res.Err != nil {
		t.Errorf("err = %v", res.Err)
	}
	// The whole line, and which pod said it. A gate that recorded only the
	// substring it was already looking for would put nothing in the audit
	// record that was not in the probe spec.
	if !strings.Contains(res.Observed, "checkout-b") || !strings.Contains(res.Observed, "upstream=payments") {
		t.Errorf("observed = %q, want the matching pod and the whole line it wrote", res.Observed)
	}
}

func TestALogsProbeThatNeverSeesItFailsWithoutErroring(t *testing.T) {
	src := &fakeLogs{
		pods: []string{"checkout-a"},
		logs: map[string]string{"checkout-a": "starting up\nlistening on :8080\n"},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1ms",
		"interval":        "1ms",
	}), logsTarget)

	if res.Passed {
		t.Fatal("probe passed against a log that does not say it")
	}
	// A probe that ran cleanly and did not see what it wanted is a verdict, not
	// a malfunction. Setting Err here would make the executor report the gate
	// as unrunnable rather than as unsatisfied.
	if res.Err != nil {
		t.Errorf("err = %v, want nil: the probe ran, it just was not satisfied", res.Err)
	}
	if !strings.Contains(res.Observed, "listening on :8080") {
		t.Errorf("observed = %q, want the last line the pod wrote", res.Observed)
	}
	if !strings.Contains(res.Expected, "upstream request failed") {
		t.Errorf("expected = %q, want it to name the string that was wanted", res.Expected)
	}
}

func TestALogsProbeKeepsPollingUntilTheLineAppears(t *testing.T) {
	src := &fakeLogs{pods: []string{"checkout-a"}, logs: map[string]string{"checkout-a": "starting up\n"}}
	src.onRead = func(round int) {
		// The workload writes its first error only after a few polls, which is
		// the normal case: the container has to start before it can complain.
		if round >= 3 {
			src.logs["checkout-a"] = "starting up\nupstream request failed\n"
		}
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "5s",
		"interval":        "1ms",
	}), logsTarget)

	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
	if res.Attempts < 3 {
		t.Errorf("attempts = %d, want the probe to have polled until the line appeared", res.Attempts)
	}
}

func TestOneUnreadablePodDoesNotSinkARoundTheOthersCanAnswer(t *testing.T) {
	src := &fakeLogs{
		pods: []string{"checkout-a", "checkout-b"},
		logs: map[string]string{"checkout-b": "upstream request failed\n"},
		// A pod that has not started its container yet cannot be read, and in a
		// multi-replica workload it is normal for one to be behind.
		errs: map[string]error{"checkout-a": fmt.Errorf("container is creating")},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1s",
	}), logsTarget)

	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
}

func TestALogsProbeThatCouldReadNothingReportsTheReadError(t *testing.T) {
	src := &fakeLogs{
		pods: []string{"checkout-a"},
		errs: map[string]error{"checkout-a": fmt.Errorf("pods \"checkout-a\" is forbidden")},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1ms",
		"interval":        "1ms",
	}), logsTarget)

	if res.Passed {
		t.Fatal("probe passed having read nothing at all")
	}
	// The distinction the Result type exists to keep: a gate that could not run
	// must not read as a fault that did not land.
	if res.Err == nil || !strings.Contains(res.Err.Error(), "forbidden") {
		t.Errorf("err = %v, want the read failure that stopped it", res.Err)
	}
}

func TestALogsProbeWithNoPodsSaysSo(t *testing.T) {
	src := &fakeLogs{}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1ms",
		"interval":        "1ms",
	}), logsTarget)

	if res.Passed {
		t.Fatal("probe passed against a namespace with no pods in it")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "app=checkout") {
		t.Errorf("err = %v, want it to name the selector that matched nothing", res.Err)
	}
}

func TestALogsProbeInheritsTheFaultsNamespaceAndSelector(t *testing.T) {
	spec, err := parseLogsSpec(map[string]any{"expect_contains": "x"}, logsTarget)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.namespace != "arena-1" {
		t.Errorf("namespace = %q, want the fault's own", spec.namespace)
	}
	// A probe that can read outside the arena is a probe that can be pointed
	// outside the arena.
	if spec.labelSelector != "app=checkout" {
		t.Errorf("selector = %q, want the fault's own", spec.labelSelector)
	}
}

func TestALogsProbeReadsTheContainerAndDepthItWasAskedFor(t *testing.T) {
	src := &fakeLogs{pods: []string{"checkout-a"}, logs: map[string]string{"checkout-a": "boom\n"}}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "boom",
		"container":       "app",
		"tail_lines":      float64(5),
		"previous":        true,
		"timeout":         "1s",
	}), logsTarget)

	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
	if len(src.seen) == 0 {
		t.Fatal("no read was made")
	}
	got := src.seen[0]
	if got.Container != "app" || got.TailLines != 5 || !got.Previous {
		t.Errorf("read options = %+v, want the spec's container, depth and previous flag", got)
	}
	// previous is the only way to see what a container said before it died, so
	// the description has to say when it is in play — a timeout message that
	// did not would send an operator looking in the wrong log.
	if !strings.Contains(res.Expected, "previous") {
		t.Errorf("expected = %q, want it to mention it read the previous instance", res.Expected)
	}
}

func TestALogsProbeDefaultsToABoundedTail(t *testing.T) {
	spec, err := parseLogsSpec(map[string]any{"expect_contains": "x"}, logsTarget)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Unbounded would pull a workload's whole log through the API server on
	// every poll, every two seconds, for the life of the gate.
	if spec.tailLines != defaultTailLines {
		t.Errorf("tail_lines = %d, want the bounded default %d", spec.tailLines, defaultTailLines)
	}
}

// The anti-vacuity rule, and the whole reason this probe type refuses to
// express an absence. strings.Contains(anything, "") is always true, and almost
// every log line contains a space, so either expectation would read like a
// check and pass unconditionally against a workload that was never created.
func TestALogsProbeRefusesAnExpectationThatMatchesEveryLog(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"absent", ""},
		{"empty", ""},
		{"space", " "},
		{"whitespace", "\t\n  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]any{}
			if tc.name != "absent" {
				raw["expect_contains"] = tc.value
			}
			_, err := parseLogsSpec(raw, logsTarget)
			if err == nil {
				t.Fatalf("expect_contains %q was accepted", tc.value)
			}
			if !strings.Contains(err.Error(), "expect_contains") {
				t.Errorf("err = %v, want it to name the field", err)
			}
		})
	}
}

func TestALogsProbeRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  map[string]any
		tgt  Target
		want string
	}{
		{
			"a pod and a selector",
			map[string]any{"expect_contains": "x", "name": "checkout-a", "label_selector": "app=checkout"},
			logsTarget, "mutually exclusive",
		},
		{
			"no namespace anywhere",
			map[string]any{"expect_contains": "x"},
			Target{Labels: map[string]string{"app": "checkout"}}, "namespace",
		},
		{
			"nothing to aim at",
			map[string]any{"expect_contains": "x"},
			Target{Namespace: "arena-1"}, "label_selector",
		},
		{"negative tail", map[string]any{"expect_contains": "x", "tail_lines": float64(-1)}, logsTarget, "tail_lines"},
		{"zero timeout", map[string]any{"expect_contains": "x", "timeout": "0s"}, logsTarget, "timeout"},
		{"zero interval", map[string]any{"expect_contains": "x", "interval": "0s"}, logsTarget, "interval"},
		{"timeout as a number", map[string]any{"expect_contains": "x", "timeout": float64(30)}, logsTarget, "duration"},
		{"container not a string", map[string]any{"expect_contains": "x", "container": float64(1)}, logsTarget, "container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLogsSpec(tc.raw, tc.tgt)
			if err == nil {
				t.Fatalf("spec %v was accepted", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A probe's Observed goes into the audit record, and a workload that logs a
// stack trace per line would otherwise put megabytes there on every poll.
func TestALogsProbeSummarizesRatherThanTranscribes(t *testing.T) {
	src := &fakeLogs{
		pods: []string{"checkout-a"},
		logs: map[string]string{"checkout-a": strings.Repeat("a line of otherwise uninteresting output\n", 500)},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1ms",
		"interval":        "1ms",
	}), logsTarget)

	if len(res.Observed) > 1024 {
		t.Errorf("observed is %d bytes; a probe verdict is not a log shipper", len(res.Observed))
	}
	if !strings.Contains(res.Observed, "uninteresting output") {
		t.Errorf("observed = %q, want the last line the pod wrote", res.Observed)
	}
}

func TestALogsProbeReadsAtMostSoManyPods(t *testing.T) {
	src := &fakeLogs{logs: map[string]string{}}
	for i := 0; i < maxLogPods*3; i++ {
		name := fmt.Sprintf("checkout-%02d", i)
		src.pods = append(src.pods, name)
		src.logs[name] = "nothing to see\n"
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"timeout":         "1ms",
		"interval":        "1ms",
	}), logsTarget)

	// A gate is looking for evidence that something in the workload is saying
	// it, not taking a census, and each pod is a separate streamed request
	// against the API server — once per poll, for the life of the gate.
	if want := maxLogPods * res.Attempts; len(src.seen) > want {
		t.Errorf("read %d pods over %d polls, want at most %d", len(src.seen), res.Attempts, want)
	}
	if len(src.seen) == 0 {
		t.Fatal("read nothing at all")
	}
}

func TestANamedPodIsReadWithoutListing(t *testing.T) {
	src := &fakeLogs{
		listErr: fmt.Errorf("list should not have been called"),
		logs:    map[string]string{"checkout-a": "upstream request failed\n"},
	}
	res := NewLogsProber(src).Run(t.Context(), logsProbe(map[string]any{
		"expect_contains": "upstream request failed",
		"name":            "checkout-a",
		"timeout":         "1s",
	}), logsTarget)

	if !res.Passed {
		t.Fatalf("probe did not pass: %s", res.Describe())
	}
}

// The production LogSource, against the fake clientset. This does not test what
// the logs say — the fake returns a fixed body — but it does prove the call
// shape compiles against client-go and that the options reach PodLogOptions.
func TestTheKubernetesLogSourceReadsThroughTheClientset(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "checkout-b", Namespace: "arena-1", Labels: map[string]string{"app": "checkout"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "checkout-a", Namespace: "arena-1", Labels: map[string]string{"app": "checkout"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "arena-1", Labels: map[string]string{"app": "elsewhere"}}},
	)
	src := &KubernetesLogSource{Clientset: cs}

	pods, err := src.PodNames(t.Context(), "arena-1", "app=checkout")
	if err != nil {
		t.Fatalf("PodNames: %v", err)
	}
	// Sorted, so a probe reads the same pods in the same order on every poll
	// and an audit record is comparable across runs.
	if len(pods) != 2 || pods[0] != "checkout-a" || pods[1] != "checkout-b" {
		t.Errorf("pods = %v, want the two selected ones in name order", pods)
	}

	if _, err := src.PodLog(t.Context(), "arena-1", "checkout-a", LogOptions{Container: "app", TailLines: 10}); err != nil {
		t.Errorf("PodLog: %v", err)
	}
}

func TestALogSourceWithoutAClientsetSaysSoRatherThanPanicking(t *testing.T) {
	src := &KubernetesLogSource{}
	if _, err := src.PodNames(t.Context(), "arena-1", "app=checkout"); err == nil {
		t.Error("PodNames on an unwired source returned no error")
	}
	if _, err := src.PodLog(t.Context(), "arena-1", "checkout-a", LogOptions{}); err == nil {
		t.Error("PodLog on an unwired source returned no error")
	}
}

// A manifest asking for a probe type the controller does not run must not be
// treated as gated. The same guard as for "cmd", now that "logs" is a type the
// catalog emits and an operator could plausibly hand-write.
func TestALogsProbeAgainstAControllerWithoutOneIsAnError(t *testing.T) {
	mux := NewMux(map[string]Prober{simian.ProbeTypeK8s: &K8sProber{}})
	res := mux.Run(t.Context(), logsProbe(map[string]any{"expect_contains": "x"}), logsTarget)
	if res.Passed {
		t.Fatal("an unrunnable probe passed")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "logs") {
		t.Errorf("err = %v, want it to name the missing prober", res.Err)
	}
}
