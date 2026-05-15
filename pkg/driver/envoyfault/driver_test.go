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

package envoyfault

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut/envoy"
)

// stubResolver returns a fixed list of pods regardless of the selector.
type stubResolver struct {
	pods []TargetPod
	err  error
}

func (s *stubResolver) ResolvePods(_ context.Context, _ string, _ map[string]string) ([]TargetPod, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.pods, nil
}

// captureClient records every HTTP request the driver issues so tests
// can assert on the URL + query params Envoy would have received.
type captureClient struct {
	mu       sync.Mutex
	requests []*http.Request
	status   int
	body     string
}

func (c *captureClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	status := c.status
	if status == 0 {
		status = 200
	}
	body := c.body
	if body == "" {
		body = "OK"
	}
	return &http.Response{
		StatusCode: status,
		Body:       http.NoBody,
		Header:     http.Header{},
		Request:    req,
		Status:     fmt.Sprintf("%d", status),
		// body unused
		ContentLength: int64(len(body)),
	}, nil
}

func (c *captureClient) Requests() []*http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*http.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

func sampleDelayManifest() simian.FaultManifest {
	return simian.FaultManifest{
		UID:          "f-test",
		Source:       simian.SourceAutonomous,
		Engine:       simian.EngineEnvoyFault,
		APIVersion:   APIVersion,
		ResourceKind: KindDelay,
		Spec: map[string]any{
			"percentage":     float64(100),
			"fixed_delay_ms": float64(250),
			"labelSelectors": map[string]any{"app": "frontend"},
		},
		Targets: []simian.TargetRef{{Namespace: "boutique-m3", Name: "frontend"}},
	}
}

func sampleAbortManifest() simian.FaultManifest {
	return simian.FaultManifest{
		UID:          "f-test",
		Source:       simian.SourceAutonomous,
		Engine:       simian.EngineEnvoyFault,
		APIVersion:   APIVersion,
		ResourceKind: KindAbort,
		Spec: map[string]any{
			"percentage":     float64(50),
			"http_status":    float64(503),
			"labelSelectors": map[string]any{"app": "frontend"},
		},
		Targets: []simian.TargetRef{{Namespace: "boutique-m3", Name: "frontend"}},
	}
}

func TestEngine(t *testing.T) {
	d := &Driver{}
	if got := d.Engine(); got != simian.EngineEnvoyFault {
		t.Errorf("Engine()=%q, want %q", got, simian.EngineEnvoyFault)
	}
}

func TestApplyDelaySendsCorrectRuntimeKeys(t *testing.T) {
	cap := &captureClient{}
	d := &Driver{
		HTTP: cap,
		Resolver: &stubResolver{pods: []TargetPod{
			{Namespace: "boutique-m3", Name: "frontend-abc", IP: "10.0.0.5"},
			{Namespace: "boutique-m3", Name: "frontend-def", IP: "10.0.0.6"},
		}},
	}
	uid, err := d.Apply(context.Background(), sampleDelayManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if uid == "" {
		t.Error("Apply should return non-empty engineUID")
	}
	reqs := cap.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 admin POSTs (one per pod); got %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST; got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/runtime_modify") {
			t.Errorf("expected /runtime_modify path; got %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.Host, "10.0.0.5") && !strings.Contains(r.URL.Host, "10.0.0.6") {
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		port := r.URL.Port()
		if port != fmt.Sprintf("%d", envoy.AdminPort) {
			t.Errorf("expected port %d; got %s", envoy.AdminPort, port)
		}
		q := r.URL.Query()
		if got := q.Get(runtimeKeyDelayPercent); got != "100" {
			t.Errorf("delay percent=%q, want 100", got)
		}
		if got := q.Get(runtimeKeyDelayDuration); got != "250" {
			t.Errorf("delay duration=%q, want 250", got)
		}
	}
}

func TestApplyAbortSendsCorrectRuntimeKeys(t *testing.T) {
	cap := &captureClient{}
	d := &Driver{
		HTTP:     cap,
		Resolver: &stubResolver{pods: []TargetPod{{Namespace: "boutique-m3", Name: "frontend-1", IP: "10.0.0.7"}}},
	}
	if _, err := d.Apply(context.Background(), sampleAbortManifest()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	q := cap.Requests()[0].URL.Query()
	if got := q.Get(runtimeKeyAbortPercent); got != "50" {
		t.Errorf("abort percent=%q, want 50", got)
	}
	if got := q.Get(runtimeKeyAbortStatus); got != "503" {
		t.Errorf("abort status=%q, want 503", got)
	}
}

func TestApplyClearRoundTrip(t *testing.T) {
	cap := &captureClient{}
	d := &Driver{
		HTTP:     cap,
		Resolver: &stubResolver{pods: []TargetPod{{Namespace: "boutique-m3", Name: "frontend-1", IP: "10.0.0.5"}}},
	}
	uid, err := d.Apply(context.Background(), sampleDelayManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	reqs := cap.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 POSTs (apply + clear); got %d", len(reqs))
	}
	clearQ := reqs[1].URL.Query()
	if got := clearQ.Get(runtimeKeyDelayPercent); got != "0" {
		t.Errorf("clear should reset delay percent to 0; got %q", got)
	}
}

func TestApplyRejectsBadKind(t *testing.T) {
	d := &Driver{HTTP: &captureClient{}, Resolver: &stubResolver{}}
	m := sampleDelayManifest()
	m.ResourceKind = "EnvoyHttpYolo"
	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject unknown kind")
	}
}

func TestApplyRejectsMissingPercentage(t *testing.T) {
	d := &Driver{HTTP: &captureClient{}, Resolver: &stubResolver{}}
	m := sampleDelayManifest()
	delete(m.Spec, "percentage")
	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject missing percentage")
	}
}

func TestApplyRejectsMissingHttpStatusForAbort(t *testing.T) {
	d := &Driver{HTTP: &captureClient{}, Resolver: &stubResolver{}}
	m := sampleAbortManifest()
	delete(m.Spec, "http_status")
	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject missing http_status for abort")
	}
}

func TestApplyRejectsMissingLabelSelectors(t *testing.T) {
	d := &Driver{HTTP: &captureClient{}, Resolver: &stubResolver{}}
	m := sampleDelayManifest()
	delete(m.Spec, "labelSelectors")
	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject missing labelSelectors")
	}
}

func TestApplyRejectsOutOfRangePercentage(t *testing.T) {
	d := &Driver{HTTP: &captureClient{}, Resolver: &stubResolver{}}
	m := sampleDelayManifest()
	m.Spec["percentage"] = float64(150)
	if _, err := d.Apply(context.Background(), m); err == nil {
		t.Error("Apply should reject percentage > 100")
	}
}

func TestApplyRejectsNoMatchingPods(t *testing.T) {
	d := &Driver{
		HTTP:     &captureClient{},
		Resolver: &stubResolver{pods: nil},
	}
	if _, err := d.Apply(context.Background(), sampleDelayManifest()); err == nil {
		t.Error("Apply should reject when no pods match selector")
	}
}

func TestApplyAgainstFakeAdminServerSucceeds(t *testing.T) {
	// Spin up an httptest server that mimics Envoy's admin /runtime_modify
	// behavior. Asserts the driver hits the endpoint with the right query.
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST; got %s", r.Method)
		}
		if r.URL.Path != "/runtime_modify" {
			t.Errorf("expected /runtime_modify; got %s", r.URL.Path)
		}
		got = r.URL.Query()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Strip http:// + parse host:port from the test server URL.
	host := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.Split(host, ":")
	if len(parts) != 2 {
		t.Fatalf("unexpected test server URL: %s", srv.URL)
	}
	var portInt int
	if _, err := fmt.Sscanf(parts[1], "%d", &portInt); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	d := &Driver{
		HTTP:      srv.Client(),
		Resolver:  &stubResolver{pods: []TargetPod{{Namespace: "boutique-m3", Name: "x", IP: parts[0]}}},
		AdminPort: portInt,
	}
	if _, err := d.Apply(context.Background(), sampleDelayManifest()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Get(runtimeKeyDelayPercent) != "100" {
		t.Errorf("admin server received wrong delay percent: %v", got)
	}
}

func TestApplyReportsAdminError(t *testing.T) {
	cap := &captureClient{status: 500, body: "no such runtime key"}
	d := &Driver{
		HTTP:     cap,
		Resolver: &stubResolver{pods: []TargetPod{{Namespace: "x", Name: "y", IP: "10.0.0.1"}}},
	}
	if _, err := d.Apply(context.Background(), sampleDelayManifest()); err == nil {
		t.Error("Apply should error when admin returns 500")
	}
}

func TestCatalogReturnsBothKinds(t *testing.T) {
	d := &Driver{}
	cat, err := d.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("expected 2 catalog entries; got %d", len(cat))
	}
	kinds := map[string]simian.CatalogEntry{}
	for _, e := range cat {
		kinds[e.ResourceKind] = e
		if e.Engine != simian.EngineEnvoyFault {
			t.Errorf("Engine=%q, want %q", e.Engine, simian.EngineEnvoyFault)
		}
		if e.BlastRadiusTier != simian.TierNamespace {
			t.Errorf("tier=%q, want %q", e.BlastRadiusTier, simian.TierNamespace)
		}
		if e.SpecTemplate == "" {
			t.Errorf("kind %q missing SpecTemplate", e.ResourceKind)
		}
		if !strings.Contains(e.SpecTemplate, "envoy=true") {
			t.Errorf("kind %q SpecTemplate should mention envoy=true precondition", e.ResourceKind)
		}
	}
	if _, ok := kinds[KindDelay]; !ok {
		t.Error("Catalog missing EnvoyHttpDelay")
	}
	if _, ok := kinds[KindAbort]; !ok {
		t.Error("Catalog missing EnvoyHttpAbort")
	}
}

func TestUIDRoundTrip(t *testing.T) {
	in := envoyFaultUID{
		Namespace:    "boutique-m3",
		LabelMatch:   map[string]string{"app": "frontend"},
		ResourceKind: KindDelay,
	}
	enc, err := encodeUID(in)
	if err != nil {
		t.Fatalf("encodeUID: %v", err)
	}
	out, err := decodeUID(enc)
	if err != nil {
		t.Fatalf("decodeUID: %v", err)
	}
	if out.Namespace != in.Namespace || out.ResourceKind != in.ResourceKind || out.LabelMatch["app"] != "frontend" {
		t.Errorf("round-trip mismatch: %+v -> %s -> %+v", in, enc, out)
	}
}

func TestDecodeUIDInvalid(t *testing.T) {
	if _, err := decodeUID("not-base64!!"); err == nil {
		t.Error("decodeUID should reject non-base64 input")
	}
}

func TestKubernetesPodResolverFiltersOnReadyAndAnnotation(t *testing.T) {
	cs := fake.NewSimpleClientset(
		// Ready + annotated → included
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "good", Namespace: "ns",
				Labels:      map[string]string{"app": "frontend"},
				Annotations: map[string]string{envoy.InjectedAnnotation: "true"},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIP:      "10.0.0.1",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
		// Missing annotation → excluded
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "noinject", Namespace: "ns",
				Labels: map[string]string{"app": "frontend"},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIP:      "10.0.0.2",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
		// Not Ready → excluded
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "starting", Namespace: "ns",
				Labels:      map[string]string{"app": "frontend"},
				Annotations: map[string]string{envoy.InjectedAnnotation: "true"},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIP:      "10.0.0.3",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			},
		},
		// Wrong label → excluded by selector
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "other", Namespace: "ns",
				Labels:      map[string]string{"app": "cart"},
				Annotations: map[string]string{envoy.InjectedAnnotation: "true"},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIP:      "10.0.0.4",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
	)
	r := &KubernetesPodResolver{Clientset: cs}
	pods, err := r.ResolvePods(context.Background(), "ns", map[string]string{"app": "frontend"})
	if err != nil {
		t.Fatalf("ResolvePods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod (good); got %d: %+v", len(pods), pods)
	}
	if pods[0].Name != "good" || pods[0].IP != "10.0.0.1" {
		t.Errorf("unexpected pod: %+v", pods[0])
	}
}
