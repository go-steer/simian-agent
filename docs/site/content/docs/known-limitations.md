---
title: "Known limitations"
linkTitle: "Known limitations"
weight: 100
description: "Cluster-side gotchas, dataplane caveats, and feature limitations contributors should know about."
---

This page is the canonical place to land if a fault "applied successfully" but didn't appear to do anything, or if a SUT pod refuses to come up after enabling Envoy injection.

## Chaos Mesh's NetworkChaos may or may not work on GKE Dataplane V2 — measure it

Historically it did not. Chaos Mesh installs a `netem` qdisc on the pod's `eth0`, which we verified is present at the kernel level; Dataplane V2 routed pod-to-pod traffic through eBPF maps that bypassed the tc qdisc layer, so the latency / loss never got applied and the `Sent ... pkt` counter on the qdisc stayed flat.

References: [chaos-mesh#3302](https://github.com/chaos-mesh/chaos-mesh/issues/3302), [cilium#19975](https://github.com/cilium/cilium/issues/19975).

**That no longer reproduces on current GKE.** Measured 2026-09-04 on a Standard cluster with `datapathProvider: ADVANCED_DATAPATH`, GKE 1.36.3-gke.1537000, Cilium v1.19.4-gke.49 (`anetd`), Chaos Mesh v2.8.2, COS nodes on kernel 6.12.94+:

| Fault | Before | During | After |
|---|---|---|---|
| `NetworkChaos` `delay` 250ms on `frontend` | ~90ms per request | ~3.9s | ~90ms |
| `NetworkChaos` `partition` both on `frontend` | HTTP 200 | connect timeout | HTTP 200 |

Both landed. The delay figure is much larger than the injected 250ms because one Online Boutique page load fans out into a dozen internal round trips and each one pays the delay twice — a useful reminder that injected latency and observed latency are not the same number.

Treat this as version-dependent rather than settled either way. Cilium, the GKE release, and the node image all move, and the failure mode when it does not work is that the fault applies cleanly and does nothing. Simian's [default efficacy gate]({{< relref "efficacy-probes.md" >}}) is what makes that safe to be uncertain about: on a cluster where the qdisc is bypassed, the delay or partition fails its Settle probe and is rolled back rather than reported as applied. To find out where your own cluster stands, apply one gated `NetworkChaos` and read the `fault.efficacy` audit record — the [GKE bring-up]({{< relref "gke-bring-up.md" >}}) page walks through exactly that.

**Alternatives, still shipped and still useful** — they do not depend on the dataplane at all, which is the point:

- The [`network-policy` engine]({{< relref "chaos-engines.md" >}}) handles partition-style chaos. Works on DPv2.
- The [`envoy-fault` engine]({{< relref "chaos-engines.md" >}}) handles HTTP-layer delay + abort via an injected Envoy sidecar. Works on DPv2 (subject to the limitation immediately below).
- The word *silently* no longer applies to any of it. `NetworkChaos` carries a [default efficacy gate]({{< relref "efficacy-probes.md" >}}): a partition or delay that the qdisc never applied fails at inject time with a named probe and is rolled back, instead of being reported as a fault the agent under test then gets scored on. Same for `NetworkPolicy` on a CNI that does not enforce it.

For non-network chaos, `PodChaos` / `StressChaos` / `TimeChaos` / `IOChaos` / `JVMChaos` continue to work fine on Dataplane V2. See [DPv2-compatible chaos engines]({{< relref "dpv2-chaos-engines.md" >}}) for the full design rationale.

## Envoy injection breaks gRPC kubelet probes

**This is why the chart default is `sutInjection.envoyFaults: false`.**

The current Envoy injection model intercepts ALL inbound TCP on the SUT-declared service ports via iptables PREROUTING REDIRECT to Envoy's listener (port 15006). Envoy speaks HTTP at the L7 layer; it does not understand gRPC health-probe payloads.

| Workload probe type | Behavior with Envoy injection |
|---|---|
| HTTP `httpGet` probes (e.g. Online Boutique `frontend`) | ✅ Works — Envoy responds to the probe |
| TCP `tcpSocket` probes (e.g. `redis-cart`) | ✅ Works — Envoy accepts the TCP handshake |
| gRPC `grpc:` probes on a redirected port (most Online Boutique services) | ❌ Probe fails → kubelet kills the container → `CrashLoopBackOff` |
| gRPC `grpc:` probes on a NON-redirected port | ✅ Works — no interception |

For Online Boutique specifically, `--no-envoy-faults=false` (i.e. injection on) leaves 9 of 12 deployments crash-looping. Until probe rewriting (Istio's `pilot-agent` style) or an outbound-only redirect mode is implemented, the rule of thumb is: only enable Envoy injection for SUTs whose probes you've audited as HTTP-only or TCP-only.

### Cheap-escape-hatch: exclude probe ports from interception

If a workload's probe port is *different* from its service port (e.g. a service on 8080 with a probe on 8081), you can exempt the probe port from the iptables redirect — kubelet's probe traffic bypasses Envoy entirely while service traffic still goes through:

```bash
# SUT-wide: exclude port 8081 from interception for every Deployment
simian sut deploy --namespace boutique-1 --no-envoy-faults=false \
                  --envoy-exclude-port=8081
```

```yaml
# Per-workload: only this Deployment exempts the listed ports
metadata:
  template:
    metadata:
      annotations:
        simian.chaos/envoy-exclude-ports: "8081,9090"
```

Or declare it on the SUT itself by implementing the `EnvoyExcludePortsProvider` interface (see `pkg/sut/sut.go`). The three layers merge.

**Caveat:** when probe port equals service port (Online Boutique's situation for most workloads), exempting the port also disables fault injection against that workload. Trade-off: "no CrashLoopBackOff" vs "no fault injection on this workload." For SUTs that need both, the full probe-rewriter (forthcoming) is the proper fix.

### Workaround for arbitrary workloads

Deploy the SUT with the default (`--no-envoy-faults=true`), then hand-author a small Deployment whose probes you control (HTTP `httpGet` or TCP `tcpSocket`), add the Envoy sidecar + iptables init + bootstrap ConfigMap from `pkg/sut/envoy/` to it, apply the `EnvoyHttpDelay` / `EnvoyHttpAbort` chaos against that pod's label selector, and measure with `curl` through the Envoy listener port (15006). See [Using the chaos engines]({{< relref "chaos-engines.md" >}}) for the `simian chaos` invocation pattern.

## Chaos Mesh on GKE Standard with Node Auto-Provisioning

The chaos-daemon DaemonSet won't land on NAP-provisioned nodes without (a) the right `default-compute-class-non-daemonset` label on the chaos-mesh namespace and (b) a `cloud.google.com/compute-class:NoSchedule` toleration. Without both, `NetworkChaos` / `IOChaos` reconciliation fails with `cannot find daemonIP on node ...`.

This is an install-time concern, not a Simian bug — but it affects every chaos-mesh-using install on GKE NAP. Documented in the README's "Known cluster-side gotchas" section.

## Autonomous LLM bias toward chaos-mesh

Without `--hypothesis-hint`, the LLM almost never picks the new `network-policy` or `envoy-fault` engines because chaos-mesh has 12+ catalog entries vs 1+2. Possible mitigations: (a) tier-policy filtering, (b) explicit per-engine "weight" in the catalog, (c) prompt rule that encourages cross-engine plans. Not blocking; the hypothesis-hint workaround is reliable.

## Metrics provider deferred

`get_metrics` returns `{"configured":false,"reason":"metrics provider not configured (deferred); see roadmap.md M3 risks."}`. The hook is wired; a real provider lands in a later milestone.
