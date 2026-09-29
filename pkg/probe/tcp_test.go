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
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func tcpProbe(spec map[string]any) simian.ProbeSpec {
	return simian.ProbeSpec{Name: "reachability", Type: simian.ProbeTypeTCP, Mode: simian.ProbeModeSOT, Spec: fastSpec(spec)}
}

// grpcLikeServer accepts connections and answers each the way a gRPC server
// does before it has read anything: with an HTTP/2 SETTINGS frame.
func grpcLikeServer(t *testing.T) (ip string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("\x00\x00\x06\x04\x00\x00\x00\x00\x00\x00\x05\x00\x00\x40\x00"))
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// The trial's paymentservice, reduced: a healthy gRPC server. The http probe
// calls it unreachable, which is what refused every NetworkChaos against
// Online Boutique; the tcp probe calls it what it is.
func TestATCPProbeReachesAServiceTheHTTPProbeCannotRead(t *testing.T) {
	ip, port := grpcLikeServer(t)
	lister := &stubLister{pods: []Pod{{Name: "paymentservice-1", IP: ip, Ports: []int{port}}}}
	p := NewHTTPProber(lister, &http.Client{Transport: &http.Transport{DisableKeepAlives: true}})

	spec := func() map[string]any { return fastSpec(map[string]any{"expect_reachable": true}) }
	if res := p.Run(context.Background(), httpProbe(spec()), webTarget()); res.Passed {
		t.Fatalf("http probe passed against a gRPC server; this test no longer shows the problem: %s", res.Describe())
	}
	res := p.TCP().Run(context.Background(), tcpProbe(spec()), webTarget())
	if !res.Passed {
		t.Fatalf("tcp probe did not reach a listening gRPC server: %s", res.Describe())
	}
	if want := "tcp://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "): connected in"; !strings.Contains(res.Observed, want) {
		t.Errorf("observed = %q, want it to contain %q", res.Observed, want)
	}
}

func TestATCPProbeSeesNothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "127.0.0.1", Ports: []int{port}}}}
	p := NewHTTPProber(lister, &stubDoer{}).TCP()
	if res := p.Run(context.Background(), tcpProbe(map[string]any{"expect_unreachable": true}), webTarget()); !res.Passed {
		t.Errorf("expect_unreachable failed against a closed port: %s", res.Describe())
	}
	if res := p.Run(context.Background(), tcpProbe(map[string]any{"expect_reachable": true}), webTarget()); res.Passed {
		t.Errorf("expect_reachable passed against a closed port: %s", res.Describe())
	}
}

// A netem delay on the target holds its SYN-ACK, so the connect is what gets
// slow — or, delayed hard enough, never completes. Both have to satisfy the
// delay gate, exactly as they do for the http probe.
func TestATCPProbeTimesTheConnect(t *testing.T) {
	stall := func(d time.Duration) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			select {
			case <-time.After(d):
				a, b := net.Pipe()
				_ = b.Close()
				return a, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	lister := &stubLister{pods: []Pod{{Name: "web-1", IP: "10.0.0.1", Ports: []int{8080}}}}

	for _, tc := range []struct {
		name     string
		connect  time.Duration
		spec     map[string]any
		wantPass bool
	}{
		{"slow connect meets min_latency", 60 * time.Millisecond,
			map[string]any{"min_latency": "40ms", "request_timeout": "1s"}, true},
		{"fast connect does not", time.Millisecond,
			map[string]any{"min_latency": "40ms", "request_timeout": "1s"}, false},
		{"a connect that never completes counts as slow", time.Hour,
			map[string]any{"min_latency": "40ms", "request_timeout": "60ms", "timeout": "100ms"}, true},
		{"fast connect meets max_latency", time.Millisecond,
			map[string]any{"expect_reachable": true, "max_latency": "40ms"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHTTPProber(lister, &stubDoer{})
			h.dial = stall(tc.connect)
			res := h.TCP().Run(context.Background(), tcpProbe(tc.spec), webTarget())
			if res.Passed != tc.wantPass {
				t.Errorf("passed = %v, want %v: %s", res.Passed, tc.wantPass, res.Describe())
			}
			if !strings.Contains(res.Expected, "connect time") && !strings.Contains(res.Expected, "connection") {
				t.Errorf("expected = %q, want it phrased as a connect", res.Expected)
			}
		})
	}
}

// A key that only means something for a response is refused, not ignored. A
// tcp probe carrying expect_status reads as a status check and makes none.
func TestATCPProbeRefusesResponseKeys(t *testing.T) {
	p := NewHTTPProber(&stubLister{}, &stubDoer{}).TCP()
	for _, key := range []string{"expect_status", "expect_contains", "path", "jsonpath"} {
		spec := map[string]any{"expect_reachable": true, key: "x"}
		if key == "expect_status" {
			spec[key] = 200
		}
		res := p.Run(context.Background(), tcpProbe(spec), webTarget())
		if res.Err == nil || !strings.Contains(res.Err.Error(), key) {
			t.Errorf("%s: err = %v, want a refusal naming it", key, res.Err)
		}
	}
}

// Every Bank of Anthos pod declares no containerPort. The Service in front of
// it still has to say where traffic goes, and that is where the port comes
// from.
func TestKubernetesPodListerTakesThePortFromTheServiceWhenThePodDeclaresNone(t *testing.T) {
	bare := listerPod("balancereader-1", corev1.PodRunning, "10.0.0.5", 0)
	bare.Spec.Containers[0].Ports = nil
	svc := func(name string, selector map[string]string, ports ...corev1.ServicePort) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "boutique"},
			Spec:       corev1.ServiceSpec{Selector: selector, Ports: ports},
		}
	}
	cs := k8sfake.NewSimpleClientset(
		bare,
		listerPod("declared-1", corev1.PodRunning, "10.0.0.6", 9090),
		svc("web", map[string]string{"app": "web"},
			corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)},
			corev1.ServicePort{Port: 81, TargetPort: intstr.FromString("metrics")}),
		svc("other", map[string]string{"app": "other"},
			corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(7777)}),
		svc("headless-no-selector", nil,
			corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(6666)}),
	)
	pods, err := (&KubernetesPodLister{Clientset: cs}).ListPods(context.Background(), "boutique", "app=web")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	got := map[string][]int{}
	for _, p := range pods {
		got[p.Name] = p.Ports
	}
	if ports := got["balancereader-1"]; len(ports) != 1 || ports[0] != 8080 {
		t.Errorf("balancereader-1 ports = %v, want [8080] from the Service that selects it", ports)
	}
	if ports := got["declared-1"]; len(ports) != 1 || ports[0] != 9090 {
		t.Errorf("declared-1 ports = %v, want its own declared [9090], not the Service's", ports)
	}
}

func TestAPodWithNoPortAnywhereSaysWhereItLooked(t *testing.T) {
	lister := &stubLister{pods: []Pod{{Name: "balancereader-1", IP: "10.0.0.5"}}}
	res := NewHTTPProber(lister, &stubDoer{}).TCP().Run(context.Background(),
		tcpProbe(map[string]any{"expect_reachable": true}), webTarget())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no Service selecting it") {
		t.Fatalf("err = %v, want it to say the Services were checked too", res.Err)
	}
}
