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
