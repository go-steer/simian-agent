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

// BootstrapConfigKey is the key under which the Envoy bootstrap YAML is
// stored in the per-namespace ConfigMap (BootstrapConfigMapName). The
// sidecar container mounts the ConfigMap and points Envoy at this file.
const BootstrapConfigKey = "envoy.yaml"

// BootstrapConfigMapName is the name of the per-namespace ConfigMap that
// carries the Envoy bootstrap config. One ConfigMap per arena namespace
// is enough — every injected Deployment shares the same bootstrap.
const BootstrapConfigMapName = "simian-envoy-bootstrap"

// AdminPort is the Envoy admin listener port. The envoyfault driver POSTs
// to /runtime_modify on this port to enable/disable the fault filter at
// chaos-time.
const AdminPort = 15000

// InboundListenerPort is the port the iptables init container redirects
// the workload's inbound TCP traffic to. Envoy listens here, applies the
// HTTP fault filter (no-op until runtime is overridden), then forwards
// to the workload's original destination via SO_ORIGINAL_DST.
const InboundListenerPort = 15006

// SidecarContainerName is the canonical name of the injected Envoy sidecar.
// Used for idempotent-injection detection (re-inject is a no-op if a
// container with this name is already present).
const SidecarContainerName = "simian-envoy-fault"

// InitContainerName is the canonical name of the iptables init container.
const InitContainerName = "simian-envoy-iptables"

// AgentContainerName is the canonical name of the probe-rewriter agent
// sidecar (cmd/simian-envoy-agent). Only injected when at least one
// container in the Deployment has a probe to rewrite.
const AgentContainerName = "simian-envoy-agent"

// AnnotationsVolumeName is the in-pod volume that mounts the
// downward-API rendering of the pod's annotations, which the agent
// reads at startup to populate its probe registry.
const AnnotationsVolumeName = "simian-envoy-agent-annotations"

// AnnotationsMountPath is where the agent looks for the downward-API
// annotations file. Must match the env-var default in
// cmd/simian-envoy-agent/main.go.
const AnnotationsMountPath = "/etc/simian-envoy-agent"

// AnnotationsFileName is the file name the downward API renders inside
// AnnotationsMountPath.
const AnnotationsFileName = "annotations"

// InjectedAnnotation is set on the pod template of injected Deployments
// so the topology discoverer can flag the workload as envoy-fault-eligible
// for the planner.
const InjectedAnnotation = "simian.chaos/envoy-injected"

// SkipInjectionAnnotation, when set to "true" on a Deployment's
// pod-template metadata.annotations, causes Inject() to leave that
// Deployment unmodified. Per-workload escape hatch matched against the
// SUT-level WithEnvoyFaults flag — set this for workloads that must not
// run a sidecar (e.g. a load generator that needs raw socket behavior).
const SkipInjectionAnnotation = "simian.chaos/no-envoy-injection"

// ExcludePortsAnnotation is a comma-separated list of TCP destination
// ports to EXEMPT from iptables PREROUTING REDIRECT on the annotated
// Deployment's pods. Useful when a workload uses a separate port for
// its kubelet probe than for its service traffic — the probe port can
// be excluded so kubelet's probes bypass Envoy entirely.
//
// Per-workload supplement to InjectOptions.ExcludePorts (which applies
// globally to every injected Deployment in the SUT). The two lists are
// merged and deduplicated.
//
// Format: "<port>[,<port>...]". Whitespace tolerated. Invalid entries
// are silently skipped to avoid breaking a deploy over a typo.
const ExcludePortsAnnotation = "simian.chaos/envoy-exclude-ports"

// Bootstrap renders the Envoy bootstrap YAML for our fault-injection
// sidecar. The config is intentionally minimal:
//
//   - Admin API on AdminPort (for the envoyfault driver's runtime overrides).
//   - One inbound listener on InboundListenerPort with an HTTP connection
//     manager that includes the fault filter (default 0% delay, 0% abort).
//   - One ORIGINAL_DST cluster so traffic forwards back to the workload.
//   - layered_runtime with an admin layer the driver mutates at chaos-time.
//
// The fault filter's static config sets default delay/abort to 0 — runtime
// overrides via the admin API drive actual chaos. Runtime keys mutated by
// the driver:
//
//	fault.http.delay.fixed_delay_percent  (int 0-100)
//	fault.http.abort.abort_percent        (int 0-100)
//	fault.http.abort.http_status          (int, e.g. 503)
//
// The fixed delay duration itself is a static `fixed_delay: 30s` baked into
// the bootstrap; overriding only the percentage flips the chaos on/off.
// (Per-fault custom durations are out of scope for v1; if needed, the
// driver can also override `fault.http.delay.fixed_duration`.)
//
// HTTP/2 + websocket upgrades are enabled so the filter applies to gRPC
// (Online Boutique services) as well as plain HTTP.
func Bootstrap() string {
	return `admin:
  address:
    socket_address:
      address: 0.0.0.0
      port_value: 15000
layered_runtime:
  layers:
    - name: admin
      admin_layer: {}
static_resources:
  listeners:
    - name: inbound
      address:
        socket_address:
          address: 0.0.0.0
          port_value: 15006
      use_original_dst: true
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress_http
                http2_protocol_options: {}
                upgrade_configs:
                  - upgrade_type: websocket
                http_filters:
                  - name: envoy.filters.http.fault
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.fault.v3.HTTPFault
                      delay:
                        fixed_delay: 30s
                        percentage:
                          numerator: 0
                          denominator: HUNDRED
                      abort:
                        http_status: 503
                        percentage:
                          numerator: 0
                          denominator: HUNDRED
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route:
                            cluster: original_dst
                            timeout: 0s
  clusters:
    - name: original_dst
      type: ORIGINAL_DST
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      original_dst_lb_config:
        use_http_header: false
`
}
