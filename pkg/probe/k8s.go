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
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/jsonpath"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// K8sProber implements the "k8s" probe type: poll a jsonpath expression over a
// Kubernetes resource until it says what we expect, or give up.
//
// This is `kubectl get <resource> -n <ns> -o jsonpath=<expr>` in a loop, which
// is not a coincidence — every settle condition the eval fixtures need is
// already written in exactly that form, and matching it means they port over
// without translation.
type K8sProber struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

// NewK8sProber constructs a prober over the given dynamic client and mapper.
func NewK8sProber(dyn dynamic.Interface, mapper meta.RESTMapper) *K8sProber {
	return &K8sProber{dyn: dyn, mapper: mapper}
}

// k8sSpec is the decoded ProbeSpec.Spec for a k8s probe.
type k8sSpec struct {
	resource      string
	namespace     string
	name          string
	labelSelector string
	jsonPath      string
	expectContain string
	expectEmpty   bool
	timeout       time.Duration
	interval      time.Duration
}

// describe renders the success condition for a timeout message.
func (s k8sSpec) describe() string {
	if s.expectEmpty {
		return "empty output"
	}
	return fmt.Sprintf("%q in output", s.expectContain)
}

// satisfied reports whether one poll's output meets the condition.
func (s k8sSpec) satisfied(out string) bool {
	if s.expectEmpty {
		return strings.TrimSpace(out) == ""
	}
	return strings.Contains(out, s.expectContain)
}

// Run implements Prober.
func (k *K8sProber) Run(ctx context.Context, p simian.ProbeSpec, target Target) Result {
	res := Result{Name: p.Name, Type: p.Type}
	spec, err := parseK8sSpec(p.Spec, target)
	if err != nil {
		res.Err = fmt.Errorf("probe %q: %w", p.Name, err)
		return res
	}
	res.Expected = spec.describe()

	gvr, err := k.resolve(spec.resource)
	if err != nil {
		res.Err = fmt.Errorf("probe %q: %w", p.Name, err)
		return res
	}

	jp := jsonpath.New(p.Name)
	// Missing keys are the normal case while waiting: a pod that has not
	// crashed yet has no state.waiting.reason at all. kubectl treats that as
	// empty output rather than an error, and so must we, or every probe would
	// fail on its first poll for the wrong reason.
	jp.AllowMissingKeys(true)
	if err := jp.Parse(spec.jsonPath); err != nil {
		res.Err = fmt.Errorf("probe %q: parse jsonpath %q: %w", p.Name, spec.jsonPath, err)
		return res
	}

	start := time.Now()
	deadline := start.Add(spec.timeout)
	var lastErr error
	for {
		res.Attempts++
		out, err := k.poll(ctx, gvr, spec, jp)
		if err != nil {
			// Keep polling through transient read errors — the resource may
			// not exist yet — but remember the last one so a probe that only
			// ever errored reports the error rather than a bare "not seen".
			lastErr = err
			res.Observed = ""
		} else {
			lastErr = nil
			res.Observed = out
			if spec.satisfied(out) {
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

// poll performs one read and renders the jsonpath over it.
func (k *K8sProber) poll(ctx context.Context, gvr schema.GroupVersionResource, spec k8sSpec, jp *jsonpath.JSONPath) (string, error) {
	var obj any
	ri := k.dyn.Resource(gvr).Namespace(spec.namespace)
	if spec.name != "" {
		u, err := ri.Get(ctx, spec.name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get %s/%s: %w", spec.namespace, spec.name, err)
		}
		obj = u.Object
	} else {
		list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: spec.labelSelector})
		if err != nil {
			return "", fmt.Errorf("list %s in %s: %w", gvr.Resource, spec.namespace, err)
		}
		// UnstructuredContent puts the items back under "items", so a
		// {.items[*]...} expression written for kubectl works unchanged.
		obj = list.UnstructuredContent()
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, obj); err != nil {
		return "", fmt.Errorf("evaluate jsonpath: %w", err)
	}
	return buf.String(), nil
}

// resolve turns a kubectl-style resource argument ("pods", "deployments.apps")
// into a GroupVersionResource, the same way kubectl does.
func (k *K8sProber) resolve(resource string) (schema.GroupVersionResource, error) {
	arg := strings.ToLower(strings.TrimSpace(resource))
	fullySpecified, gr := schema.ParseResourceArg(arg)
	if fullySpecified != nil {
		if gvr, err := k.mapper.ResourceFor(*fullySpecified); err == nil {
			return gvr, nil
		}
	}
	gvr, err := k.mapper.ResourceFor(gr.WithVersion(""))
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("unknown resource %q: %w", resource, err)
	}
	return gvr, nil
}

// parseK8sSpec decodes and validates a k8s probe's Spec map.
func parseK8sSpec(raw map[string]any, target Target) (k8sSpec, error) {
	s := k8sSpec{
		timeout:  DefaultTimeout,
		interval: DefaultInterval,
	}
	var err error
	if s.resource, err = optString(raw, "resource"); err != nil {
		return s, err
	}
	if s.namespace, err = optString(raw, "namespace"); err != nil {
		return s, err
	}
	if s.name, err = optString(raw, "name"); err != nil {
		return s, err
	}
	if s.labelSelector, err = optString(raw, "label_selector"); err != nil {
		return s, err
	}
	if s.jsonPath, err = optString(raw, "jsonpath"); err != nil {
		return s, err
	}
	if s.expectContain, err = optString(raw, "expect_contains"); err != nil {
		return s, err
	}
	if s.expectEmpty, err = optBool(raw, "expect_empty"); err != nil {
		return s, err
	}
	if s.timeout, err = optDuration(raw, "timeout", s.timeout); err != nil {
		return s, err
	}
	if s.interval, err = optDuration(raw, "interval", s.interval); err != nil {
		return s, err
	}

	if s.resource == "" {
		return s, fmt.Errorf("k8s probe: %q is required", "resource")
	}
	if s.jsonPath == "" {
		return s, fmt.Errorf("k8s probe: %q is required", "jsonpath")
	}
	if s.namespace == "" {
		s.namespace = target.Namespace
	}
	if s.namespace == "" {
		return s, fmt.Errorf("k8s probe: no namespace, and the fault declares no target namespace to fall back to")
	}
	if s.name != "" && s.labelSelector != "" {
		return s, fmt.Errorf("k8s probe: %q and %q are mutually exclusive", "name", "label_selector")
	}
	if s.name == "" && s.labelSelector == "" {
		// Aim at whatever the fault aims at. This is what lets a default probe
		// be written once per fault kind instead of once per manifest.
		s.labelSelector = target.Selector()
	}

	// A probe must state a condition, and "expect_contains": "" is not one:
	// strings.Contains(anything, "") is always true, so it would read like a
	// check while passing unconditionally. That is the exact failure this
	// package exists to catch, so it is rejected rather than tolerated.
	switch {
	case s.expectEmpty && s.expectContain != "":
		return s, fmt.Errorf("k8s probe: %q and %q are mutually exclusive", "expect_empty", "expect_contains")
	case !s.expectEmpty && s.expectContain == "":
		return s, fmt.Errorf("k8s probe: needs %q (non-empty) or %q: a probe with neither passes unconditionally",
			"expect_contains", "expect_empty")
	}
	if s.timeout <= 0 {
		return s, fmt.Errorf("k8s probe: %q must be positive, got %s", "timeout", s.timeout)
	}
	if s.interval <= 0 {
		return s, fmt.Errorf("k8s probe: %q must be positive, got %s", "interval", s.interval)
	}
	return s, nil
}
