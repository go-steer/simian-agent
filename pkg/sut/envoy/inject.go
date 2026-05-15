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

// Package envoy injects an Envoy-fault sidecar + iptables init container
// into SUT Deployments at deploy time. Used by pkg/sut/manager.Deploy
// when the operator opts into Envoy-based HTTP fault injection (default
// on; opt out per SUT with --no-envoy-faults or per workload with the
// SkipInjectionAnnotation).
//
// The injection is the producer side of the envoy-fault chaos engine
// (pkg/driver/envoyfault). The driver assumes any pod with annotation
// InjectedAnnotation has an Envoy admin API reachable on AdminPort and
// the fault filter pre-wired with default-zero percentages.
package envoy

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Default container images. These are baked-in so a fresh checkout works
// without per-cluster configuration; production installs can override via
// InjectOptions.
const (
	DefaultEnvoyImage    = "envoyproxy/envoy:v1.31-latest"
	DefaultIptablesImage = "nicolaka/netshoot:latest"
)

// volumeName is the name of the in-pod volume that mounts the Envoy
// bootstrap ConfigMap. Stable so pods that already declare a volume of
// this name are left untouched (idempotent re-injection).
const volumeName = "simian-envoy-bootstrap"

// bootstrapMountPath is where the Envoy sidecar reads its bootstrap from.
const bootstrapMountPath = "/etc/simian-envoy"

// InjectOptions configures how Inject() mutates SUT manifests.
type InjectOptions struct {
	// Ports is the list of TCP destination ports the iptables init
	// container redirects to Envoy's inbound listener. These should be
	// the SUT's actual service ports (gRPC, HTTP, etc.). Empty means
	// "redirect nothing" — the sidecar is still injected (so the driver
	// has somewhere to send fault config) but no traffic is intercepted.
	Ports []int

	// EnvoyImage overrides DefaultEnvoyImage.
	EnvoyImage string

	// IptablesImage overrides DefaultIptablesImage.
	IptablesImage string
}

// Inject mutates the docs slice: for each Deployment that does not carry
// SkipInjectionAnnotation, adds the Envoy sidecar + iptables init container
// + bootstrap-ConfigMap volume mount + InjectedAnnotation. If at least one
// Deployment was injected, also prepends a ConfigMap document carrying
// the Envoy bootstrap so the volume mounts have something to read.
//
// Returns a new slice; the input docs are not modified beyond their pod
// templates (the per-doc *unstructured.Unstructured is mutated in place
// for already-present docs, but the slice may include a freshly created
// ConfigMap at index 0).
//
// Idempotent: if a Deployment already carries a container named
// SidecarContainerName, it is left untouched.
func Inject(docs []*unstructured.Unstructured, opts InjectOptions) ([]*unstructured.Unstructured, error) {
	if opts.EnvoyImage == "" {
		opts.EnvoyImage = DefaultEnvoyImage
	}
	if opts.IptablesImage == "" {
		opts.IptablesImage = DefaultIptablesImage
	}
	// Sort+dedupe ports for stable iptables command + tests.
	ports := uniqueSortedPorts(opts.Ports)

	injectedAny := false
	for _, doc := range docs {
		if doc.GetKind() != "Deployment" {
			continue
		}
		mutated, err := injectDeployment(doc, ports, opts)
		if err != nil {
			return nil, fmt.Errorf("envoy inject %s: %w", doc.GetName(), err)
		}
		if mutated {
			injectedAny = true
		}
	}

	if !injectedAny {
		return docs, nil
	}

	cm, err := bootstrapConfigMap()
	if err != nil {
		return nil, err
	}
	// Prepend so the ConfigMap is applied before any Deployment that mounts it.
	out := make([]*unstructured.Unstructured, 0, len(docs)+1)
	out = append(out, cm)
	out = append(out, docs...)
	return out, nil
}

// injectDeployment mutates doc's pod template in place. Returns true if
// injection happened, false if the Deployment was skipped (already
// injected, or carries SkipInjectionAnnotation).
func injectDeployment(doc *unstructured.Unstructured, ports []int, opts InjectOptions) (bool, error) {
	// Decode the pod template to typed core/v1 for ergonomics.
	rawTemplate, found, err := unstructured.NestedMap(doc.Object, "spec", "template")
	if err != nil {
		return false, fmt.Errorf("read spec.template: %w", err)
	}
	if !found {
		// Not all Deployments have a template (shouldn't happen for valid
		// SUTs, but be defensive).
		return false, nil
	}
	var tpl corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rawTemplate, &tpl); err != nil {
		return false, fmt.Errorf("decode pod template: %w", err)
	}

	// Per-workload skip annotation.
	if v, ok := tpl.Annotations[SkipInjectionAnnotation]; ok && v == "true" {
		return false, nil
	}
	// Idempotency: if our sidecar is already present, leave alone.
	for _, c := range tpl.Spec.Containers {
		if c.Name == SidecarContainerName {
			return false, nil
		}
	}

	// Annotation flag for the topology discoverer.
	if tpl.Annotations == nil {
		tpl.Annotations = map[string]string{}
	}
	tpl.Annotations[InjectedAnnotation] = "true"

	// Mount the bootstrap ConfigMap.
	tpl.Spec.Volumes = appendVolumeIfMissing(tpl.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: BootstrapConfigMapName},
			},
		},
	})

	// Init container that installs the iptables redirect rules.
	tpl.Spec.InitContainers = appendContainerIfMissing(tpl.Spec.InitContainers, makeIptablesInitContainer(ports, opts.IptablesImage))

	// Sidecar.
	tpl.Spec.Containers = append(tpl.Spec.Containers, makeEnvoySidecar(opts.EnvoyImage))

	// Encode back into the doc.
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&tpl)
	if err != nil {
		return false, fmt.Errorf("encode pod template: %w", err)
	}
	if err := unstructured.SetNestedMap(doc.Object, encoded, "spec", "template"); err != nil {
		return false, fmt.Errorf("set spec.template: %w", err)
	}
	return true, nil
}

// makeIptablesInitContainer returns the init container that redirects
// inbound TCP for the configured ports to Envoy's inbound listener. Uses
// the netshoot image (well-known network-debug image with iptables-legacy
// + iptables-nft both available). Runs as root with NET_ADMIN.
//
// Why root: modern kernels (nf_tables backend) require uid 0 to modify
// the rule set even when NET_ADMIN is granted. The Online Boutique
// upstream manifests set a pod-level securityContext.runAsNonRoot=true
// + runAsUser=1000, so we explicitly override here to avoid
// "Could not fetch rule set generation id: Permission denied" at init
// time. The override only applies to this single init container; the
// workload containers continue to run under their original
// non-root context.
//
// The redirect is INBOUND: traffic destined for the workload's service
// ports lands in Envoy first. Envoy applies the fault filter (when
// runtime overrides it) and then forwards to the original destination
// (the workload's actual port) via its ORIGINAL_DST cluster.
func makeIptablesInitContainer(ports []int, image string) corev1.Container {
	cmd := buildIptablesScript(ports)
	root := int64(0)
	noRoot := false
	return corev1.Container{
		Name:    InitContainerName,
		Image:   image,
		Command: []string{"sh", "-c", cmd},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
			RunAsUser:    &root,
			RunAsNonRoot: &noRoot,
		},
	}
}

// buildIptablesScript renders the iptables commands. One -j REDIRECT per
// port; if no ports are given the script is a no-op (Envoy still injected
// but nothing intercepted — useful for testing the injection path).
func buildIptablesScript(ports []int) string {
	if len(ports) == 0 {
		return "echo 'simian-envoy: no ports configured; iptables redirect skipped'"
	}
	var sb strings.Builder
	sb.WriteString("set -eux\n")
	for _, p := range ports {
		fmt.Fprintf(&sb, "iptables -t nat -A PREROUTING -p tcp --dport %d -j REDIRECT --to-port %d\n", p, InboundListenerPort)
	}
	return sb.String()
}

// makeEnvoySidecar returns the Envoy container spec. Reads its bootstrap
// from the mounted ConfigMap and exposes the admin port for the
// envoyfault driver to reach via pod-IP HTTP.
func makeEnvoySidecar(image string) corev1.Container {
	return corev1.Container{
		Name:  SidecarContainerName,
		Image: image,
		Args: []string{
			"-c", bootstrapMountPath + "/" + BootstrapConfigKey,
			"--service-cluster", "simian-envoy",
			"--service-node", "simian-envoy",
		},
		Ports: []corev1.ContainerPort{
			{Name: "envoy-admin", ContainerPort: AdminPort, Protocol: corev1.ProtocolTCP},
			{Name: "envoy-in", ContainerPort: InboundListenerPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeName, MountPath: bootstrapMountPath, ReadOnly: true},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt(AdminPort),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		},
	}
}

// bootstrapConfigMap returns an unstructured ConfigMap carrying the Envoy
// bootstrap YAML. The ConfigMap is namespace-less here; pkg/sut.Manager
// stamps the namespace before applying.
func bootstrapConfigMap() (*unstructured.Unstructured, error) {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: BootstrapConfigMapName,
			Labels: map[string]string{
				"simian.chaos/managed": "true",
			},
		},
		Data: map[string]string{
			BootstrapConfigKey: Bootstrap(),
		},
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cm)
	if err != nil {
		return nil, fmt.Errorf("encode bootstrap ConfigMap: %w", err)
	}
	return &unstructured.Unstructured{Object: encoded}, nil
}

func appendContainerIfMissing(list []corev1.Container, c corev1.Container) []corev1.Container {
	for _, existing := range list {
		if existing.Name == c.Name {
			return list
		}
	}
	return append(list, c)
}

func appendVolumeIfMissing(list []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for _, existing := range list {
		if existing.Name == v.Name {
			return list
		}
	}
	return append(list, v)
}

func uniqueSortedPorts(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, p := range in {
		if p <= 0 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
