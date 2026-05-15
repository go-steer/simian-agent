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

// Package envoyfault implements simian.ChaosDriver for HTTP-layer faults
// delivered via the Envoy sidecar injected by pkg/sut/envoy. The driver
// pokes each target pod's Envoy admin API to enable / disable runtime
// overrides of the fault filter — chaos comes from the same long-lived
// sidecar that's pre-baked into the SUT, no chaos resources are created
// in Kubernetes.
//
// The engine ships two kinds:
//
//   - EnvoyHttpDelay: inject a fixed delay into a percentage of inbound
//     HTTP/gRPC requests to the workload's Envoy sidecar.
//   - EnvoyHttpAbort: return an HTTP error status for a percentage of
//     inbound requests.
//
// Both kinds require the target workload to have the Envoy sidecar
// injected (annotation simian.chaos/envoy-injected=true on the pod).
// Phase 3c will surface this on the topology snapshot so the planner
// only proposes envoy-fault chaos against eligible workloads. Until
// then, applying against an uninjected workload returns a clear error
// from the Apply call (driver.failed in the audit stream).
package envoyfault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut/envoy"
)

// KindDelay is the catalog ResourceKind for HTTP-delay faults.
const KindDelay = "EnvoyHttpDelay"

// KindAbort is the catalog ResourceKind for HTTP-abort faults.
const KindAbort = "EnvoyHttpAbort"

// APIVersion is a virtual API version stamp; envoy-fault is not a CRD,
// but the manifest schema requires an api_version field. We use a
// simian.io vendor namespace to make the non-Kubernetes nature obvious.
const APIVersion = "simian.io/v1"

// HTTPClient is the minimal HTTP interface the driver depends on.
// Real callers pass an *http.Client; tests can pass anything that
// satisfies Do().
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// PodResolver returns the set of pods matching a namespace + label
// selector. Implemented by the production-path KubernetesPodResolver
// (wraps clientset); tests can pass a stub.
type PodResolver interface {
	ResolvePods(ctx context.Context, namespace string, labelSelectors map[string]string) ([]TargetPod, error)
}

// TargetPod is the per-pod context the driver needs to talk to that
// pod's Envoy admin API.
type TargetPod struct {
	Namespace string
	Name      string
	IP        string
}

// Driver implements simian.ChaosDriver for the envoy-fault engine.
type Driver struct {
	HTTP     HTTPClient
	Resolver PodResolver
	// AdminPort overrides envoy.AdminPort if set. Useful for tests that
	// run a fake admin server on a non-standard port.
	AdminPort int
}

// New creates a Driver wired to the given Kubernetes clientset. The
// production path uses an http.Client with a 5s timeout per pod call.
func New(clientset kubernetes.Interface) *Driver {
	return &Driver{
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Resolver: &KubernetesPodResolver{Clientset: clientset},
	}
}

// Engine implements ChaosDriver.
func (d *Driver) Engine() simian.Engine { return simian.EngineEnvoyFault }

// Apply implements ChaosDriver. Resolves target pods by labelSelector,
// then POSTs to each pod's Envoy admin /runtime_modify endpoint to
// enable the fault filter for the manifest's duration.
//
// Returns an engineUID encoding the manifest's namespace + selector +
// kind so Clear can re-resolve pods (handles pod churn during the
// fault) and undo the runtime overrides.
func (d *Driver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if len(m.Targets) == 0 {
		return "", fmt.Errorf("envoy-fault apply: manifest has no targets")
	}
	ns := m.Targets[0].Namespace
	if ns == "" {
		return "", fmt.Errorf("envoy-fault apply: manifest target has no namespace")
	}
	labels, err := extractLabelSelectors(m.Spec)
	if err != nil {
		return "", fmt.Errorf("envoy-fault apply: %w", err)
	}

	overrides, err := buildOverrides(m.ResourceKind, m.Spec)
	if err != nil {
		return "", fmt.Errorf("envoy-fault apply: %w", err)
	}

	pods, err := d.Resolver.ResolvePods(ctx, ns, labels)
	if err != nil {
		return "", fmt.Errorf("envoy-fault apply: resolve pods: %w", err)
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("envoy-fault apply: no envoy-injected pods match labelSelector %v in namespace %q", labels, ns)
	}

	if err := d.fanout(ctx, pods, overrides); err != nil {
		return "", fmt.Errorf("envoy-fault apply: %w", err)
	}

	uid, err := encodeUID(envoyFaultUID{
		Namespace:    ns,
		LabelMatch:   labels,
		ResourceKind: m.ResourceKind,
	})
	if err != nil {
		return "", fmt.Errorf("envoy-fault apply: encode uid: %w", err)
	}
	return uid, nil
}

// Clear implements ChaosDriver. Decodes the engineUID, re-resolves pods
// (so pod churn during the fault is handled correctly), and POSTs the
// reset overrides for the kind.
func (d *Driver) Clear(ctx context.Context, engineUIDStr string) error {
	uid, err := decodeUID(engineUIDStr)
	if err != nil {
		return fmt.Errorf("envoy-fault clear: %w", err)
	}
	pods, err := d.Resolver.ResolvePods(ctx, uid.Namespace, uid.LabelMatch)
	if err != nil {
		return fmt.Errorf("envoy-fault clear: resolve pods: %w", err)
	}
	if len(pods) == 0 {
		// Pods may have been torn down before clear ran (e.g. SUT
		// destroyed). Treat as success — there's nothing left to undo.
		return nil
	}
	resets := buildResets(uid.ResourceKind)
	if err := d.fanout(ctx, pods, resets); err != nil {
		return fmt.Errorf("envoy-fault clear: %w", err)
	}
	return nil
}

// Catalog implements ChaosDriver. Returns one entry per supported kind.
// envoy-fault is not a CRD — entries are always present regardless of
// cluster state, but applying succeeds only against pods that actually
// have the Envoy sidecar injected (see Apply's pod-resolver check).
func (d *Driver) Catalog(_ context.Context) ([]simian.CatalogEntry, error) {
	return []simian.CatalogEntry{
		{
			Engine:          simian.EngineEnvoyFault,
			APIVersion:      APIVersion,
			ResourceKind:    KindDelay,
			BlastRadiusTier: catalog.Classify(simian.EngineEnvoyFault, KindDelay),
			Description:     "Inject a fixed delay into HTTP/gRPC requests at the workload's Envoy sidecar (works on GKE Dataplane V2).",
			SpecTemplate: `Adds a fixed delay to a percentage of inbound HTTP/gRPC requests at
the target workload's Envoy sidecar. Works on GKE Dataplane V2.

REQUIRES: target workload must be flagged envoy=true in the topology
snapshot. If the workload was deployed without --no-envoy-faults, the
sidecar is present and this fault will fire; otherwise apply returns
an error.

Spec:
  {"percentage": 100, "fixed_delay_ms": 250,
   "labelSelectors": {"app": "<workload>"}}

  - percentage: required, integer 0-100. Fraction of requests to delay.
  - fixed_delay_ms: optional, integer milliseconds (default 30000 from
    bootstrap). The driver overrides Envoy's runtime to this value.
  - labelSelectors: required pod label match.`,
		},
		{
			Engine:          simian.EngineEnvoyFault,
			APIVersion:      APIVersion,
			ResourceKind:    KindAbort,
			BlastRadiusTier: catalog.Classify(simian.EngineEnvoyFault, KindAbort),
			Description:     "Return an HTTP error status for a percentage of inbound requests at the workload's Envoy sidecar.",
			SpecTemplate: `Returns an HTTP error status for a percentage of inbound HTTP/gRPC
requests at the target workload's Envoy sidecar. Works on GKE
Dataplane V2.

REQUIRES: target workload must be flagged envoy=true in the topology
snapshot. If the workload was deployed without --no-envoy-faults, the
sidecar is present and this fault will fire; otherwise apply returns
an error.

Spec:
  {"percentage": 100, "http_status": 503,
   "labelSelectors": {"app": "<workload>"}}

  - percentage: required, integer 0-100. Fraction of requests to abort.
  - http_status: required, integer (e.g. 500, 503).
  - labelSelectors: required pod label match.`,
		},
	}, nil
}

// fanout calls /runtime_modify on every pod's Envoy admin API with the
// given overrides. Errors from individual pods are collected and
// returned as a single joined error if any fail; partial successes are
// not rolled back (the caller — typically Clear — re-runs to converge).
func (d *Driver) fanout(ctx context.Context, pods []TargetPod, overrides map[string]string) error {
	port := d.AdminPort
	if port == 0 {
		port = envoy.AdminPort
	}
	var errs []string
	for _, p := range pods {
		if err := d.modifyOne(ctx, p, port, overrides); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", p.Namespace, p.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("runtime_modify failed for %d pod(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// modifyOne POSTs a single /runtime_modify call to one pod's Envoy
// admin API. The body is empty; the overrides ride as URL query params,
// per the Envoy admin API contract.
func (d *Driver) modifyOne(ctx context.Context, pod TargetPod, port int, overrides map[string]string) error {
	if pod.IP == "" {
		return fmt.Errorf("pod %s has no IP", pod.Name)
	}
	q := url.Values{}
	for k, v := range overrides {
		q.Set(k, v)
	}
	endpoint := fmt.Sprintf("http://%s:%d/runtime_modify?%s", pod.IP, port, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("admin returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// buildOverrides translates a manifest spec into the runtime keys to
// set on each pod's Envoy. Returns an error for invalid kinds or
// missing required fields.
func buildOverrides(kind string, spec map[string]any) (map[string]string, error) {
	out := map[string]string{}
	switch kind {
	case KindDelay:
		pct, err := requireIntPercent(spec, "percentage")
		if err != nil {
			return nil, err
		}
		out[runtimeKeyDelayPercent] = strconv.Itoa(pct)
		if v, ok := spec["fixed_delay_ms"]; ok {
			ms, err := toInt(v)
			if err != nil {
				return nil, fmt.Errorf("spec.fixed_delay_ms: %w", err)
			}
			if ms < 0 {
				return nil, fmt.Errorf("spec.fixed_delay_ms must be non-negative, got %d", ms)
			}
			out[runtimeKeyDelayDuration] = strconv.Itoa(ms)
		}
	case KindAbort:
		pct, err := requireIntPercent(spec, "percentage")
		if err != nil {
			return nil, err
		}
		out[runtimeKeyAbortPercent] = strconv.Itoa(pct)
		statusRaw, ok := spec["http_status"]
		if !ok {
			return nil, fmt.Errorf("spec.http_status is required for EnvoyHttpAbort")
		}
		status, err := toInt(statusRaw)
		if err != nil {
			return nil, fmt.Errorf("spec.http_status: %w", err)
		}
		if status < 100 || status > 599 {
			return nil, fmt.Errorf("spec.http_status must be 100-599, got %d", status)
		}
		out[runtimeKeyAbortStatus] = strconv.Itoa(status)
	default:
		return nil, fmt.Errorf("unknown resource_kind %q (valid: %s, %s)", kind, KindDelay, KindAbort)
	}
	return out, nil
}

// buildResets returns the runtime overrides that disable the fault for
// the given kind (set the percentage to 0).
func buildResets(kind string) map[string]string {
	switch kind {
	case KindDelay:
		return map[string]string{runtimeKeyDelayPercent: "0"}
	case KindAbort:
		return map[string]string{runtimeKeyAbortPercent: "0"}
	default:
		return map[string]string{}
	}
}

func requireIntPercent(spec map[string]any, key string) (int, error) {
	v, ok := spec[key]
	if !ok {
		return 0, fmt.Errorf("spec.%s is required", key)
	}
	n, err := toInt(v)
	if err != nil {
		return 0, fmt.Errorf("spec.%s: %w", key, err)
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("spec.%s must be 0-100, got %d", key, n)
	}
	return n, nil
}

// toInt accepts JSON-decoded numeric values (which arrive as float64)
// or explicit ints/strings and returns the int value.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int32:
		return int(n), nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("not an integer: %q", n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

func extractLabelSelectors(spec map[string]any) (map[string]string, error) {
	raw, ok := spec["labelSelectors"]
	if !ok {
		return nil, fmt.Errorf("spec.labelSelectors is required")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec.labelSelectors must be an object")
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("spec.labelSelectors must not be empty")
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("spec.labelSelectors[%q] must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

// envoyFaultUID is the structured engineUID the driver returns from
// Apply and decodes in Clear. Encoded via base64url(JSON) to keep it
// audit-safe and tractable on the executor's lease registry.
type envoyFaultUID struct {
	Namespace    string            `json:"ns"`
	LabelMatch   map[string]string `json:"l"`
	ResourceKind string            `json:"k"`
}

func encodeUID(u envoyFaultUID) (string, error) {
	b, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeUID(s string) (envoyFaultUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return envoyFaultUID{}, fmt.Errorf("invalid engineUID encoding: %w", err)
	}
	var u envoyFaultUID
	if err := json.Unmarshal(b, &u); err != nil {
		return envoyFaultUID{}, fmt.Errorf("invalid engineUID JSON: %w", err)
	}
	return u, nil
}

// KubernetesPodResolver implements PodResolver against a real cluster.
// Only returns pods that:
//   - Have a non-empty PodIP.
//   - Are in the Running phase with the Ready condition true.
//   - Carry the simian.chaos/envoy-injected=true annotation (so we
//     don't accidentally POST to a non-Envoy pod's port 15000).
type KubernetesPodResolver struct {
	Clientset kubernetes.Interface
}

// ResolvePods implements PodResolver. The label selector is built from
// the labels map by joining "k=v" with commas — standard K8s syntax.
func (r *KubernetesPodResolver) ResolvePods(ctx context.Context, namespace string, labels map[string]string) ([]TargetPod, error) {
	if r.Clientset == nil {
		return nil, fmt.Errorf("envoy-fault: pod resolver has no clientset")
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts) // deterministic for tests
	sel := strings.Join(parts, ",")
	list, err := r.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list pods (selector=%q): %w", sel, err)
	}
	out := make([]TargetPod, 0, len(list.Items))
	for _, p := range list.Items {
		if !podIsReady(p) || p.Status.PodIP == "" {
			continue
		}
		if v := p.Annotations[envoy.InjectedAnnotation]; v != "true" {
			continue
		}
		out = append(out, TargetPod{
			Namespace: p.Namespace,
			Name:      p.Name,
			IP:        p.Status.PodIP,
		})
	}
	return out, nil
}

func podIsReady(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
