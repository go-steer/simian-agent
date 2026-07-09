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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// makeDeployment returns an unstructured Deployment skeleton with one
// "app" container, suitable for injection tests.
func makeDeployment(name string, podAnnotations map[string]string) *unstructured.Unstructured {
	tpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: podAnnotations,
			Labels:      map[string]string{"app": name},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "ghcr.io/example/" + name + ":1.0"},
			},
		},
	}
	tplMap, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&tpl)
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{
				"replicas": int64(1),
				"template": tplMap,
			},
		},
	}
}

func makeService(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]any{"name": name},
		},
	}
}

func decodeTemplate(t *testing.T, doc *unstructured.Unstructured) corev1.PodTemplateSpec {
	t.Helper()
	rawTpl, found, err := unstructured.NestedMap(doc.Object, "spec", "template")
	if err != nil || !found {
		t.Fatalf("decode pod template: found=%v err=%v", found, err)
	}
	var tpl corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawTpl, &tpl); err != nil {
		t.Fatalf("FromUnstructured: %v", err)
	}
	return tpl
}

func TestInjectAddsSidecarAndInitContainer(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeployment("frontend", nil),
		makeService("frontend"),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{80, 8080}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// Bootstrap ConfigMap prepended.
	if len(out) != 3 {
		t.Fatalf("expected 3 docs (cm + dep + svc), got %d", len(out))
	}
	if out[0].GetKind() != "ConfigMap" || out[0].GetName() != BootstrapConfigMapName {
		t.Errorf("first doc should be the bootstrap ConfigMap; got kind=%q name=%q",
			out[0].GetKind(), out[0].GetName())
	}
	// Deployment was mutated.
	dep := out[1]
	if dep.GetKind() != "Deployment" || dep.GetName() != "frontend" {
		t.Fatalf("expected Deployment frontend at index 1; got %s/%s",
			dep.GetKind(), dep.GetName())
	}
	tpl := decodeTemplate(t, dep)

	if got := tpl.Annotations[InjectedAnnotation]; got != "true" {
		t.Errorf("InjectedAnnotation should be true; got %q", got)
	}
	if !hasContainer(tpl.Spec.Containers, SidecarContainerName) {
		t.Error("Envoy sidecar container missing from injected pod")
	}
	if !hasContainer(tpl.Spec.InitContainers, InitContainerName) {
		t.Error("iptables init container missing from injected pod")
	}
	// App container preserved.
	if !hasContainer(tpl.Spec.Containers, "app") {
		t.Error("original 'app' container was removed by injection")
	}
	// Volume mounted.
	hasVol := false
	for _, v := range tpl.Spec.Volumes {
		if v.Name == volumeName {
			if v.ConfigMap == nil || v.ConfigMap.Name != BootstrapConfigMapName {
				t.Errorf("bootstrap volume should reference %s; got %+v", BootstrapConfigMapName, v.ConfigMap)
			}
			hasVol = true
		}
	}
	if !hasVol {
		t.Error("bootstrap ConfigMap volume missing from pod spec")
	}
	// Init container security context has NET_ADMIN.
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
			t.Fatal("init container should have security context with capabilities")
		}
		hasNetAdmin := false
		for _, cap := range c.SecurityContext.Capabilities.Add {
			if cap == "NET_ADMIN" {
				hasNetAdmin = true
			}
		}
		if !hasNetAdmin {
			t.Errorf("init container should add NET_ADMIN; got %+v", c.SecurityContext.Capabilities.Add)
		}
		// Must run as root: nf_tables ops require uid 0 even with NET_ADMIN,
		// and the SUT's pod-level runAsNonRoot=true would otherwise apply.
		if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
			t.Errorf("init container should set RunAsUser=0 to override pod-level non-root; got %+v", c.SecurityContext.RunAsUser)
		}
		if c.SecurityContext.RunAsNonRoot == nil || *c.SecurityContext.RunAsNonRoot != false {
			t.Errorf("init container should set RunAsNonRoot=false; got %+v", c.SecurityContext.RunAsNonRoot)
		}
		// Script should reference both ports.
		if len(c.Command) < 3 {
			t.Fatal("init container command should be sh -c <script>")
		}
		script := c.Command[2]
		if !strings.Contains(script, "--dport 80") {
			t.Errorf("script should redirect port 80; got:\n%s", script)
		}
		if !strings.Contains(script, "--dport 8080") {
			t.Errorf("script should redirect port 8080; got:\n%s", script)
		}
		if !strings.Contains(script, "--to-port 15006") {
			t.Errorf("script should target Envoy listener port 15006; got:\n%s", script)
		}
	}
	// Sidecar exposes admin + inbound ports.
	for _, c := range tpl.Spec.Containers {
		if c.Name != SidecarContainerName {
			continue
		}
		hasAdmin, hasIn := false, false
		for _, p := range c.Ports {
			if p.ContainerPort == AdminPort {
				hasAdmin = true
			}
			if p.ContainerPort == InboundListenerPort {
				hasIn = true
			}
		}
		if !hasAdmin || !hasIn {
			t.Errorf("sidecar should expose admin + inbound ports; got %+v", c.Ports)
		}
	}
}

func TestInjectIsIdempotent(t *testing.T) {
	docs := []*unstructured.Unstructured{makeDeployment("frontend", nil)}
	once, err := Inject(docs, InjectOptions{Ports: []int{80}})
	if err != nil {
		t.Fatalf("Inject (1): %v", err)
	}
	twice, err := Inject(once, InjectOptions{Ports: []int{80}})
	if err != nil {
		t.Fatalf("Inject (2): %v", err)
	}
	// Re-injection should not double-prepend the ConfigMap (the first
	// pass already mutated the Deployment to include the sidecar; the
	// second pass detects the sidecar and skips, so injectedAny is false
	// and no ConfigMap is prepended).
	if len(twice) != len(once) {
		t.Errorf("re-injection should be a no-op; got %d -> %d docs", len(once), len(twice))
	}
	tpl := decodeTemplate(t, twice[1])
	count := 0
	for _, c := range tpl.Spec.Containers {
		if c.Name == SidecarContainerName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("re-injection should not duplicate sidecar; got %d sidecars", count)
	}
}

func TestInjectHonorsPerWorkloadSkipAnnotation(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeployment("frontend", map[string]string{SkipInjectionAnnotation: "true"}),
		makeDeployment("cartservice", nil),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{80}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// frontend skipped, cartservice injected. ConfigMap prepended.
	if len(out) != 3 {
		t.Fatalf("expected 3 docs (cm + 2 deployments); got %d", len(out))
	}
	frontend := findDeployment(out, "frontend")
	if frontend == nil {
		t.Fatal("frontend Deployment missing from output")
	}
	tpl := decodeTemplate(t, frontend)
	if hasContainer(tpl.Spec.Containers, SidecarContainerName) {
		t.Error("frontend has skip annotation; should not have been injected")
	}

	cart := findDeployment(out, "cartservice")
	if cart == nil {
		t.Fatal("cartservice Deployment missing from output")
	}
	cartTpl := decodeTemplate(t, cart)
	if !hasContainer(cartTpl.Spec.Containers, SidecarContainerName) {
		t.Error("cartservice should have been injected")
	}
}

func TestInjectSkipsNoInjectWorkloads(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeployment("loadgenerator", nil),
		makeDeployment("redis-cart", nil),
		makeDeployment("cartservice", nil),
	}
	out, err := Inject(docs, InjectOptions{
		Ports:             []int{80},
		NoInjectWorkloads: []string{"loadgenerator", "redis-cart"},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// cm + 3 deployments. loadgenerator + redis-cart are skipped
	// (pod template unmodified), only cartservice gets the sidecar.
	if len(out) != 4 {
		t.Fatalf("expected 4 docs (cm + 3 deployments); got %d", len(out))
	}
	for _, name := range []string{"loadgenerator", "redis-cart"} {
		d := findDeployment(out, name)
		if d == nil {
			t.Fatalf("%s Deployment missing from output", name)
		}
		tpl := decodeTemplate(t, d)
		if hasContainer(tpl.Spec.Containers, SidecarContainerName) {
			t.Errorf("%s was in NoInjectWorkloads; should not have Envoy sidecar", name)
		}
		if _, ok := tpl.Annotations[InjectedAnnotation]; ok {
			t.Errorf("%s was in NoInjectWorkloads; should not carry the injected annotation", name)
		}
	}
	cart := findDeployment(out, "cartservice")
	if cart == nil {
		t.Fatal("cartservice Deployment missing from output")
	}
	cartTpl := decodeTemplate(t, cart)
	if !hasContainer(cartTpl.Spec.Containers, SidecarContainerName) {
		t.Error("cartservice was not in NoInjectWorkloads; should have been injected")
	}
}

func TestInjectSkipsNonDeploymentDocs(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeService("redis"),
		makeService("frontend"),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{80}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// No Deployments → no ConfigMap prepended, no mutations.
	if len(out) != 2 {
		t.Errorf("non-Deployment-only input should pass through unchanged; got %d docs", len(out))
	}
}

func TestInjectDedupesAndSortsPorts(t *testing.T) {
	docs := []*unstructured.Unstructured{makeDeployment("x", nil)}
	out, _ := Inject(docs, InjectOptions{Ports: []int{8080, 80, 8080, 0, 70000}})
	tpl := decodeTemplate(t, out[1])
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		script := c.Command[2]
		// Sorted ascending: 80 should appear before 8080.
		i80 := strings.Index(script, "--dport 80 ")
		i8080 := strings.Index(script, "--dport 8080 ")
		if i80 < 0 || i8080 < 0 {
			t.Fatalf("script missing port: %s", script)
		}
		if i80 > i8080 {
			t.Errorf("ports should be sorted ascending; got 80 after 8080:\n%s", script)
		}
		if strings.Count(script, "--dport 8080") != 1 {
			t.Errorf("port 8080 should be deduplicated; got:\n%s", script)
		}
		if strings.Contains(script, "--dport 70000") {
			t.Errorf("invalid port 70000 should be rejected; got:\n%s", script)
		}
	}
}

func TestInjectEmptyPortListSkipsRedirect(t *testing.T) {
	docs := []*unstructured.Unstructured{makeDeployment("x", nil)}
	out, _ := Inject(docs, InjectOptions{Ports: nil})
	tpl := decodeTemplate(t, out[1])
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		if !strings.Contains(c.Command[2], "iptables redirect skipped") {
			t.Errorf("empty port list should skip iptables; got:\n%s", c.Command[2])
		}
	}
}

// TestInjectExcludePortsEmitsReturnRulesFirst verifies that excluded
// ports get RETURN rules emitted BEFORE the REDIRECT rules — order
// matters in PREROUTING because nf_tables walks the chain in order
// and short-circuits on the first matching rule.
func TestInjectExcludePortsEmitsReturnRulesFirst(t *testing.T) {
	docs := []*unstructured.Unstructured{makeDeployment("x", nil)}
	out, err := Inject(docs, InjectOptions{
		Ports:        []int{80, 8080},
		ExcludePorts: []int{5050},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		script := c.Command[2]
		returnIdx := strings.Index(script, "--dport 5050 -j RETURN")
		redirect80Idx := strings.Index(script, "--dport 80 -j REDIRECT")
		if returnIdx < 0 || redirect80Idx < 0 {
			t.Fatalf("missing expected rules in script:\n%s", script)
		}
		if returnIdx > redirect80Idx {
			t.Errorf("RETURN rule for excluded port must come before REDIRECT rules; got:\n%s", script)
		}
	}
}

// TestInjectMergesPerWorkloadExcludeAnnotation verifies that the
// per-Deployment exclude annotation is added to the global exclude list.
func TestInjectMergesPerWorkloadExcludeAnnotation(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeployment("x", map[string]string{ExcludePortsAnnotation: "9090, 9091"}),
	}
	out, err := Inject(docs, InjectOptions{
		Ports:        []int{80},
		ExcludePorts: []int{5050},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		script := c.Command[2]
		for _, want := range []string{
			"--dport 5050 -j RETURN", // global
			"--dport 9090 -j RETURN", // per-workload
			"--dport 9091 -j RETURN", // per-workload, whitespace-tolerant parse
			"--dport 80 -j REDIRECT", // service traffic still intercepted
		} {
			if !strings.Contains(script, want) {
				t.Errorf("script missing %q:\n%s", want, script)
			}
		}
	}
}

// TestInjectExcludeOnlySkipsIptablesEntirely is a regression guard:
// when only ExcludePorts is set (no Ports), there's nothing to
// intercept — the script should still emit the RETURN rules but the
// no-op short-circuit at the top of buildIptablesScript shouldn't
// fire (it only fires when BOTH lists are empty).
func TestInjectExcludeOnlyStillEmitsRules(t *testing.T) {
	docs := []*unstructured.Unstructured{makeDeployment("x", nil)}
	out, err := Inject(docs, InjectOptions{ExcludePorts: []int{5050}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])
	for _, c := range tpl.Spec.InitContainers {
		if c.Name != InitContainerName {
			continue
		}
		script := c.Command[2]
		if !strings.Contains(script, "--dport 5050 -j RETURN") {
			t.Errorf("exclude-only config should emit the RETURN rule; got:\n%s", script)
		}
		if strings.Contains(script, "iptables redirect skipped") {
			t.Errorf("exclude-only config should NOT short-circuit to skipped; got:\n%s", script)
		}
	}
}

func TestParseExcludePortsAnnotation(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"5050", []int{5050}},
		{"5050,8080", []int{5050, 8080}},
		{" 5050 , 8080 ", []int{5050, 8080}}, // whitespace tolerated
		{"5050,abc,8080", []int{5050, 8080}}, // invalid silently skipped
		{",,5050,,", []int{5050}},            // empty entries skipped
	}
	for _, tc := range cases {
		got := parseExcludePortsAnnotation(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parse(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parse(%q)[%d] = %d, want %d", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// makeDeploymentWithProbe builds a Deployment whose single container
// has the named kind of probe. Used by probe-rewrite tests.
func makeDeploymentWithProbe(name string, kind ProbeKind, probe *corev1.Probe) *unstructured.Unstructured {
	tpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/example/" + name + ":1.0"}},
		},
	}
	switch kind {
	case ProbeLiveness:
		tpl.Spec.Containers[0].LivenessProbe = probe
	case ProbeReadiness:
		tpl.Spec.Containers[0].ReadinessProbe = probe
	case ProbeStartup:
		tpl.Spec.Containers[0].StartupProbe = probe
	}
	tplMap, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&tpl)
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": name},
			"spec":       map[string]any{"replicas": int64(1), "template": tplMap},
		},
	}
}

// TestInjectRewritesGRPCProbeAndAddsAgentSidecar is the core end-to-end
// regression: a Deployment with a gRPC probe goes through the injector
// and comes out with (a) the probe rewritten to httpGet against the
// agent port, (b) the original spec stashed as a pod annotation, (c)
// the agent sidecar added, (d) the downward-API annotations volume
// mounted, (e) the agent's listener port in the iptables exclude list.
func TestInjectRewritesGRPCProbeAndAddsAgentSidecar(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeploymentWithProbe("cartservice", ProbeLiveness, &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{Port: 7070},
			},
			TimeoutSeconds: 2,
		}),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{7070}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])

	// (a) Probe rewritten to httpGet against the agent.
	c := &tpl.Spec.Containers[0]
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil {
		t.Fatalf("probe not rewritten to httpGet: %+v", c.LivenessProbe)
	}
	if c.LivenessProbe.GRPC != nil {
		t.Errorf("original gRPC probe should have been cleared after rewrite")
	}
	if c.LivenessProbe.HTTPGet.Port.IntVal != ProbeRewriterPort {
		t.Errorf("rewritten probe port = %d, want %d", c.LivenessProbe.HTTPGet.Port.IntVal, ProbeRewriterPort)
	}
	if c.LivenessProbe.HTTPGet.Path != "/app-health/server/liveness" {
		t.Errorf("rewritten probe path = %q, want /app-health/server/liveness", c.LivenessProbe.HTTPGet.Path)
	}
	if c.LivenessProbe.TimeoutSeconds != 2 {
		t.Errorf("timing fields should be preserved on rewrite; got TimeoutSeconds=%d", c.LivenessProbe.TimeoutSeconds)
	}

	// (b) Original spec stashed as annotation.
	stashed, ok := tpl.Annotations[ProbeAnnotationKey("server", ProbeLiveness)]
	if !ok {
		t.Fatalf("stashed probe annotation missing; got annotations %v", tpl.Annotations)
	}
	decoded, err := UnmarshalStashedProbe(stashed)
	if err != nil {
		t.Fatalf("decode stashed probe: %v", err)
	}
	if decoded.GRPC == nil || decoded.GRPC.Port != 7070 {
		t.Errorf("stashed probe lost gRPC port: %+v", decoded)
	}

	// (c) Agent sidecar added.
	if !hasContainer(tpl.Spec.Containers, AgentContainerName) {
		t.Error("simian-envoy-agent sidecar missing")
	}

	// (d) Downward-API annotations volume mounted.
	hasAnnVol := false
	for _, v := range tpl.Spec.Volumes {
		if v.Name == AnnotationsVolumeName {
			if v.DownwardAPI == nil || len(v.DownwardAPI.Items) == 0 {
				t.Errorf("annotations volume should use DownwardAPI; got %+v", v.VolumeSource)
			}
			hasAnnVol = true
		}
	}
	if !hasAnnVol {
		t.Error("downward-API annotations volume missing")
	}

	// (e) Agent's listener port excluded from iptables redirect.
	for _, ic := range tpl.Spec.InitContainers {
		if ic.Name != InitContainerName {
			continue
		}
		script := ic.Command[2]
		excludeRule := strconv.Itoa(ProbeRewriterPort) + " -j RETURN"
		if !strings.Contains(script, excludeRule) {
			t.Errorf("iptables script should exempt agent port %d; got:\n%s", ProbeRewriterPort, script)
		}
	}
}

// TestInjectDisableProbeRewriteSkipsAgent guards the opt-out: when
// DisableProbeRewrite is set, probes pass through untouched and no
// agent sidecar / downward-API volume is added.
func TestInjectDisableProbeRewriteSkipsAgent(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeploymentWithProbe("cartservice", ProbeLiveness, &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{Port: 7070},
			},
		}),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{7070}, DisableProbeRewrite: true})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])
	if tpl.Spec.Containers[0].LivenessProbe.GRPC == nil {
		t.Error("DisableProbeRewrite should leave gRPC probe in place")
	}
	if hasContainer(tpl.Spec.Containers, AgentContainerName) {
		t.Error("DisableProbeRewrite should NOT add the agent sidecar")
	}
}

// TestInjectSkipsProbeRewriteForUnsupportedKinds — exec probes can't
// be reconstituted by the agent, so the injector should leave them in
// place. If a container has ONLY an exec probe, the agent sidecar
// shouldn't be added (nothing for it to serve).
func TestInjectSkipsProbeRewriteForExecProbe(t *testing.T) {
	docs := []*unstructured.Unstructured{
		makeDeploymentWithProbe("worker", ProbeLiveness, &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"healthcheck.sh"}},
			},
		}),
	}
	out, err := Inject(docs, InjectOptions{Ports: []int{8080}})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	tpl := decodeTemplate(t, out[1])
	if tpl.Spec.Containers[0].LivenessProbe.Exec == nil {
		t.Error("exec probe should be left in place (agent doesn't support exec)")
	}
	if hasContainer(tpl.Spec.Containers, AgentContainerName) {
		t.Error("agent sidecar should NOT be added when no probes were rewritten")
	}
}

func TestBootstrapHasFaultFilter(t *testing.T) {
	bs := Bootstrap()
	if !strings.Contains(bs, "envoy.filters.http.fault") {
		t.Error("bootstrap should include the HTTP fault filter")
	}
	if !strings.Contains(bs, "envoy.filters.http.router") {
		t.Error("bootstrap should include the router filter (terminal)")
	}
	if !strings.Contains(bs, "ORIGINAL_DST") {
		t.Error("bootstrap should use ORIGINAL_DST cluster for transparent pass-through")
	}
	if !strings.Contains(bs, "http2_protocol_options") {
		t.Error("bootstrap should enable HTTP/2 (gRPC support)")
	}
	if !strings.Contains(bs, "use_downstream_protocol_config") {
		t.Error("bootstrap should configure upstream protocol via use_downstream_protocol_config so gRPC (HTTP/2) callers get HTTP/2 upstream instead of a silent downgrade to HTTP/1.1")
	}
	if !strings.Contains(bs, "envoy.extensions.upstreams.http.v3.HttpProtocolOptions") {
		t.Error("bootstrap's ORIGINAL_DST cluster should carry typed_extension_protocol_options for HttpProtocolOptions")
	}
}

func hasContainer(list []corev1.Container, name string) bool {
	for _, c := range list {
		if c.Name == name {
			return true
		}
	}
	return false
}

func findDeployment(docs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	for _, d := range docs {
		if d.GetKind() == "Deployment" && d.GetName() == name {
			return d
		}
	}
	return nil
}
