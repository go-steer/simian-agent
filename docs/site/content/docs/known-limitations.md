---
title: "Known limitations"
linkTitle: "Known limitations"
weight: 100
description: "Cluster-side gotchas, dataplane caveats, and feature limitations contributors should know about."
---

This page is the canonical place to land if a fault "applied successfully" but didn't appear to do anything, or if a SUT pod refuses to come up after enabling Envoy injection.

## GKE Dataplane V2 silently breaks Chaos Mesh's NetworkChaos

Chaos Mesh installs a `netem` qdisc on the pod's `eth0`, which we verified is present at the kernel level. But Dataplane V2 routes pod-to-pod traffic through eBPF maps that bypass the tc qdisc layer, so the latency / loss never gets applied. The `Sent ... pkt` counter on the qdisc stays flat. This is a Chaos Mesh + Cilium incompatibility, not a Simian bug.

References: [chaos-mesh#3302](https://github.com/chaos-mesh/chaos-mesh/issues/3302), [cilium#19975](https://github.com/cilium/cilium/issues/19975) — both open since 2022, no fix in sight.

**Workarounds shipped:**

- The [`network-policy` engine]({{< relref "chaos-engines.md" >}}) handles partition-style chaos. Works on DPv2.
- The [`envoy-fault` engine]({{< relref "chaos-engines.md" >}}) handles HTTP-layer delay + abort via an injected Envoy sidecar. Works on DPv2 (subject to the limitation immediately below).

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

For Online Boutique specifically, `--no-envoy-faults=false` (i.e. injection on) leaves 9 of 12 deployments crash-looping. Until probe rewriting (Istio's `pilot-agent` style) or an outbound-only redirect mode is implemented, only enable Envoy injection for SUTs whose probes you've audited as HTTP-only or TCP-only.

Workaround for testing envoy-fault against an arbitrary workload: deploy the SUT with the default (`--no-envoy-faults=true`), then manually inject Envoy into a single test Deployment whose probes you control. The DPv2 acceptance results (in the repo root as `acceptance-m3b-results.md`, untracked) include an end-to-end recipe under "DPv2 chaos engines acceptance — round 3".

## Chaos Mesh on GKE Standard with Node Auto-Provisioning

The chaos-daemon DaemonSet won't land on NAP-provisioned nodes without (a) the right `default-compute-class-non-daemonset` label on the chaos-mesh namespace and (b) a `cloud.google.com/compute-class:NoSchedule` toleration. Without both, `NetworkChaos` / `IOChaos` reconciliation fails with `cannot find daemonIP on node ...`.

This is an install-time concern, not a Simian bug — but it affects every chaos-mesh-using install on GKE NAP. Documented in the README's "Known cluster-side gotchas" section.

## `--spec-file` CLI flag binding bug

Both `--spec` and `--spec-file` on `simian chaos` bind to the same variable, so `--spec-file=/tmp/x.json` puts the path string into the spec field and JSON-decode fails on the leading `/`. Workaround: use `--spec '<inline JSON>'` or `--stdin-spec`. Cosmetic fix; tracked.

## Autonomous LLM bias toward chaos-mesh

Without `--hypothesis-hint`, the LLM almost never picks the new `network-policy` or `envoy-fault` engines because chaos-mesh has 12+ catalog entries vs 1+2. Possible mitigations: (a) tier-policy filtering, (b) explicit per-engine "weight" in the catalog, (c) prompt rule that encourages cross-engine plans. Not blocking; the hypothesis-hint workaround is reliable.

## Metrics provider deferred

`get_metrics` returns `{"configured":false,"reason":"metrics provider not configured (deferred); see roadmap.md M3 risks."}`. The hook is wired; a real provider lands in a later milestone.
