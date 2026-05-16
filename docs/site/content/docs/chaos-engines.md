---
title: "Using the chaos engines"
linkTitle: "Chaos engines"
weight: 70
description: "Directed and autonomous patterns for chaos-mesh, network-policy, and envoy-fault."
---

Simian ships three chaos engines. Each is a `simian.ChaosDriver` registered with the executor; the LLM sees all of them via the catalog mechanism and can pick whichever fits the plan.

| Engine | What it does | When to use it |
|---|---|---|
| `chaos-mesh` | The full Chaos Mesh CRD catalog: PodChaos, StressChaos, IOChaos, TimeChaos, NetworkChaos, etc. | Default for non-network chaos. NetworkChaos is silently bypassed on GKE Dataplane V2 — see [Known limitations]({{< relref "known-limitations.md" >}}). |
| `network-policy` | Standard `networking.k8s.io/v1` NetworkPolicy partitions (deny ingress / egress / both). | Network partition chaos on GKE DPv2 (or any cluster where NetworkChaos isn't reliable). Partition only — no delay / loss / jitter. |
| `envoy-fault` | HTTP-layer delay + abort via an injected Envoy sidecar. Two kinds: `EnvoyHttpDelay`, `EnvoyHttpAbort`. | HTTP/gRPC delay or error injection on DPv2. Requires the SUT to be deployed with `--no-envoy-faults=false` (off by default — see [Known limitations]({{< relref "known-limitations.md" >}}#envoy-injection-breaks-grpc-kubelet-probes)). |

## Directed-control patterns

All three engines accept the same `simian chaos --engine ... --kind ... --spec '<inline JSON>'` shape:

```bash
# chaos-mesh: kill one paymentservice pod for 30s
simian chaos --engine chaos-mesh \
  --kind PodChaos --api-version chaos-mesh.org/v1alpha1 \
  --namespace boutique-1 --workload paymentservice --duration 30s \
  --spec '{"action":"pod-kill","mode":"one","selector":{"namespaces":["boutique-1"],"labelSelectors":{"app":"paymentservice"}}}'

# network-policy: 60s ingress+egress partition of cartservice
simian chaos --engine network-policy \
  --kind NetworkPolicy --api-version networking.k8s.io/v1 \
  --namespace boutique-1 --workload cartservice --duration 60s \
  --spec '{"labelSelectors":{"app":"cartservice"},"directions":["ingress","egress"]}'

# envoy-fault: 60s 300ms delay on 100% of inbound HTTP/gRPC requests to frontend
# (requires the workload to have been deployed with --no-envoy-faults=false
# AND to be HTTP-probed or TCP-probed)
simian chaos --engine envoy-fault \
  --kind EnvoyHttpDelay --api-version simian.io/v1 \
  --namespace boutique-1 --workload frontend --duration 60s \
  --spec '{"percentage":100,"fixed_delay_ms":300,"labelSelectors":{"app":"frontend"}}'

# envoy-fault: 60s 503 abort on 100% of inbound requests
simian chaos --engine envoy-fault \
  --kind EnvoyHttpAbort --api-version simian.io/v1 \
  --namespace boutique-1 --workload frontend --duration 60s \
  --spec '{"percentage":100,"http_status":503,"labelSelectors":{"app":"frontend"}}'
```

For the LLM-translated path:

```bash
simian chaos --intent "kill one paymentservice pod in boutique-1 for 30 seconds" \
             --namespace boutique-1
```

The LLM picks an engine + kind + spec from the catalog and emits a `FaultManifest` that the executor validates and applies. The intent must name the namespace (or the LLM's `default_namespace` arg has to carry it) — empty namespaces are rejected at the safety stage.

## Autonomous mode

The LLM has a strong bias toward Chaos Mesh's larger catalog. To exercise the new engines reliably in autonomous mode, pass an explicit hypothesis hint:

```bash
simian serve --autonomous --autonomous-namespace boutique-1 \
  --hypothesis-hint "Verify alternative chaos engines work. Test network-policy
                     to partition a service, and envoy-fault for HTTP delay/abort
                     against any workload flagged envoy=true in topology."
```

The autonomous loop's per-cycle caps (`--max-faults-per-cycle`, `--max-severity-per-cycle`, `--max-concurrent-faults`, `--min-cooldown`) apply to plans regardless of engine choice.

## Inspecting + clearing

```bash
simian chaos --list-active                 # all leased faults across engines
simian chaos --list-catalog                # catalog the LLM sees
simian chaos --clear f-<UID>               # clear before lease expiry
```

## Background reading

- [DPv2-compatible chaos engines]({{< relref "dpv2-chaos-engines.md" >}}) — full design rationale for `network-policy` and `envoy-fault`.
- [Known limitations]({{< relref "known-limitations.md" >}}) — the GKE DPv2 NetworkChaos bypass and the Envoy injection / gRPC probe interaction.
