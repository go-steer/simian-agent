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
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/simian"
)

const (
	// defaultTailLines is how much of each pod's log one poll reads. Enough to
	// span a few of a stalling app's retry intervals, small enough that polling
	// every two seconds is not a load-bearing read against the kubelet.
	defaultTailLines = 200

	// logBodyLimit caps the bytes taken from one pod even if it wrote them all
	// on one line. tail_lines bounds lines, not length, and a container that
	// dumps a stack trace or a base64 blob per line would otherwise pull
	// megabytes through the API server on every poll.
	logBodyLimit = 256 * 1024

	// maxLogPods caps how many pods one poll reads. A logs gate is looking for
	// evidence that *something* in the workload is saying it, not for a census,
	// and each pod costs a separate streamed request.
	maxLogPods = 10
)

// LogOptions is the slice of corev1.PodLogOptions a logs probe uses.
type LogOptions struct {
	// Container to read. Empty means the pod's only container, and is an error
	// against a pod that has more than one — the same rule kubectl logs uses.
	Container string

	// TailLines bounds how far back the read goes.
	TailLines int64

	// Previous reads the log of the previous instance of the container rather
	// than the running one. This is the only way to see what a container said
	// before it died, which for a restarting workload is the whole story.
	Previous bool
}

// LogSource is the minimal surface a logs probe needs. Real callers pass a
// clientset-backed implementation; tests pass a stub.
type LogSource interface {
	// PodNames returns the pods a logs probe should read, in a stable order.
	PodNames(ctx context.Context, namespace, labelSelector string) ([]string, error)

	// PodLog returns the tail of one pod's log.
	PodLog(ctx context.Context, namespace, pod string, opts LogOptions) (string, error)
}

// LogsProber implements the "logs" probe type: read the target's own logs and
// assert that something is in them.
//
// This is the probe type for the faults that leave every field correct. A
// workload whose upstream dependency has stopped answering is Running, Ready
// and Available, its Service has endpoints, and nothing in the API server is
// wrong — the failure exists only in what the application says about itself.
// Without a probe that can read that, such a fault could be applied and
// reported as landed with no evidence at all, which is the case this package
// exists to refuse.
type LogsProber struct {
	src LogSource
}

// NewLogsProber constructs a prober over the given log source.
func NewLogsProber(src LogSource) *LogsProber { return &LogsProber{src: src} }

// NewKubernetesLogsProber is the production wiring: read pod logs through the
// API server, which proxies to the kubelet.
func NewKubernetesLogsProber(cs kubernetes.Interface) *LogsProber {
	return NewLogsProber(&KubernetesLogSource{Clientset: cs})
}

// KubernetesLogSource reads pod logs through the API server.
type KubernetesLogSource struct {
	Clientset kubernetes.Interface
}

// PodNames implements LogSource.
//
// Unlike the http prober's lister this does not filter on phase or pod IP. A
// pod that is Pending or has already terminated still has a log, and for a
// crash-looping workload that log is the only place the reason is written.
func (l *KubernetesLogSource) PodNames(ctx context.Context, namespace, labelSelector string) ([]string, error) {
	if l.Clientset == nil {
		return nil, fmt.Errorf("logs probe: log source has no clientset")
	}
	list, err := l.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s (selector=%q): %w", namespace, labelSelector, err)
	}
	out := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out, nil
}

// PodLog implements LogSource.
func (l *KubernetesLogSource) PodLog(ctx context.Context, namespace, pod string, opts LogOptions) (string, error) {
	if l.Clientset == nil {
		return "", fmt.Errorf("logs probe: log source has no clientset")
	}
	o := &corev1.PodLogOptions{Container: opts.Container, Previous: opts.Previous}
	if opts.TailLines > 0 {
		tail := opts.TailLines
		o.TailLines = &tail
	}
	rc, err := l.Clientset.CoreV1().Pods(namespace).GetLogs(pod, o).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read log of %s/%s: %w", namespace, pod, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, logBodyLimit))
	if err != nil {
		return "", fmt.Errorf("read log of %s/%s: %w", namespace, pod, err)
	}
	return string(b), nil
}

// logsSpec is the decoded ProbeSpec.Spec for a logs probe.
type logsSpec struct {
	namespace     string
	labelSelector string
	podName       string
	container     string
	tailLines     int64
	previous      bool

	expectContains string

	timeout  time.Duration
	interval time.Duration
}

func (s logsSpec) describe() string {
	where := "the target's logs"
	if s.container != "" {
		where = fmt.Sprintf("container %q's logs", s.container)
	}
	if s.previous {
		where = "the previous instance of " + where
	}
	return fmt.Sprintf("%q in %s", s.expectContains, where)
}

// Run implements Prober.
func (l *LogsProber) Run(ctx context.Context, p simian.ProbeSpec, target Target) Result {
	res := Result{Name: p.Name, Type: p.Type}
	spec, err := parseLogsSpec(p.Spec, target)
	if err != nil {
		res.Err = fmt.Errorf("probe %q: %w", p.Name, err)
		return res
	}
	res.Expected = spec.describe()

	start := time.Now()
	deadline := start.Add(spec.timeout)
	var lastErr error
	for {
		res.Attempts++
		observed, matched, err := l.round(ctx, spec)
		if err != nil {
			// Keep polling through read errors. A pod that has not started its
			// container yet answers ContainerCreating rather than an empty log,
			// and that is the normal first poll, not a broken probe.
			lastErr = err
			res.Observed = ""
		} else {
			lastErr = nil
			res.Observed = observed
			if matched {
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

// round reads one poll's worth of logs and reports whether any pod said it.
//
// The observed value is a summary, not the log. A gate that dumped 200 lines
// per pod into every audit record would make the record unreadable and the
// probe's own verdict hard to find; the last line each pod wrote is what an
// operator looks at first anyway.
func (l *LogsProber) round(ctx context.Context, spec logsSpec) (string, bool, error) {
	pods := []string{spec.podName}
	if spec.podName == "" {
		var err error
		pods, err = l.src.PodNames(ctx, spec.namespace, spec.labelSelector)
		if err != nil {
			return "", false, err
		}
		if len(pods) == 0 {
			return "", false, fmt.Errorf("no pods in %s matching %q", spec.namespace, spec.labelSelector)
		}
		if len(pods) > maxLogPods {
			pods = pods[:maxLogPods]
		}
	}

	opts := LogOptions{Container: spec.container, TailLines: spec.tailLines, Previous: spec.previous}
	var (
		summary  []string
		firstErr error
		read     int
	)
	for _, pod := range pods {
		out, err := l.src.PodLog(ctx, spec.namespace, pod, opts)
		if err != nil {
			// One unreadable pod does not fail the round — in a multi-replica
			// workload the others may already be saying it. An error only
			// surfaces if no pod could be read at all.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		read++
		if strings.Contains(out, spec.expectContains) {
			return fmt.Sprintf("%s: %s", pod, matchingLine(out, spec.expectContains)), true, nil
		}
		if last := lastLine(out); last != "" {
			summary = append(summary, fmt.Sprintf("%s: %s", pod, last))
		}
	}
	if read == 0 {
		return "", false, firstErr
	}
	return truncate(strings.Join(summary, "; "), 512), false, nil
}

// matchingLine returns the whole log line the match fell on, so the audit
// record carries the evidence in the form it was written rather than the
// substring the gate happened to be looking for.
func matchingLine(out, want string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return truncate(line, 512)
		}
	}
	// The match spanned a line boundary. Rare, and not worth a second scan.
	return truncate(want, 512)
}

// lastLine returns the final non-blank line of a log.
func lastLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// parseLogsSpec decodes and validates a logs probe's Spec map.
func parseLogsSpec(raw map[string]any, target Target) (logsSpec, error) {
	s := logsSpec{
		tailLines: defaultTailLines,
		timeout:   DefaultTimeout,
		interval:  DefaultInterval,
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
	if s.container, err = optString(raw, "container"); err != nil {
		return s, err
	}
	if s.expectContains, err = optString(raw, "expect_contains"); err != nil {
		return s, err
	}
	if s.previous, err = optBool(raw, "previous"); err != nil {
		return s, err
	}
	if n, err := optInt(raw, "tail_lines"); err != nil {
		return s, err
	} else if n != 0 {
		s.tailLines = int64(n)
	}
	if s.timeout, err = optDuration(raw, "timeout", s.timeout); err != nil {
		return s, err
	}
	if s.interval, err = optDuration(raw, "interval", s.interval); err != nil {
		return s, err
	}

	if s.namespace == "" {
		s.namespace = target.Namespace
	}
	if s.namespace == "" {
		return s, fmt.Errorf("logs probe: no namespace, and the fault declares no target namespace to fall back to")
	}
	if s.podName != "" && s.labelSelector != "" {
		return s, fmt.Errorf("logs probe: %q and %q are mutually exclusive", "name", "label_selector")
	}
	if s.podName == "" && s.labelSelector == "" {
		s.labelSelector = target.Selector()
	}
	if s.podName == "" && s.labelSelector == "" {
		return s, fmt.Errorf("logs probe: needs %q or %q, and the fault declares no target labels to fall back to", "name", "label_selector")
	}

	// The anti-vacuity rule, and the reason this probe type has no
	// "expect_empty" counterpart to the k8s probe's.
	//
	// strings.Contains(anything, "") is always true, so an empty expectation
	// reads like a check and passes unconditionally. Whitespace is refused for
	// the same reason with one more step: almost every log line contains a
	// space.
	//
	// The absent case is worse and is not offered at all. "The log does not say
	// X" is satisfied by a container that has not started, by a pod that was
	// never created, and by a typo in the string — it is the vacuous pass with
	// no way to tell it from a real one, so a logs probe can only assert a
	// presence.
	if strings.TrimSpace(s.expectContains) == "" {
		return s, fmt.Errorf("logs probe: %q must be non-empty and not just whitespace: every log contains \"\" and almost every log contains \" \"", "expect_contains")
	}
	if s.tailLines <= 0 {
		return s, fmt.Errorf("logs probe: %q must be positive, got %d", "tail_lines", s.tailLines)
	}
	if s.timeout <= 0 {
		return s, fmt.Errorf("logs probe: %q must be positive, got %s", "timeout", s.timeout)
	}
	if s.interval <= 0 {
		return s, fmt.Errorf("logs probe: %q must be positive, got %s", "interval", s.interval)
	}
	return s, nil
}
