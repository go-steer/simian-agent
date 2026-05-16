---
title: "Helm values reference"
linkTitle: "Helm values"
weight: 90
description: "Every Helm chart value, what it does, and the recommended setting."
---

The chart is at `deploy/helm/simian/`. Reference for every value lives in the chart's [`values.yaml`](https://github.com/go-steer/simian-agent/blob/main/deploy/helm/simian/values.yaml) — that file has long inline comments explaining each setting and is the canonical source. This page summarizes by category.

For installs that want a known-good starting point rather than the chart defaults, layer the [recommended overlay](https://github.com/go-steer/simian-agent/blob/main/examples/values-baked-defaults.yaml) on top.

## Image

| Value | Default | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/go-steer/simian-agent` | Published by the release workflow on every `v*` tag. |
| `image.tag` | `""` (falls back to `Chart.AppVersion`) | Pin explicitly for production so chart upgrades don't silently change the running binary. |
| `image.pullPolicy` | `IfNotPresent` | |

## Eligibility

| Value | Default | Notes |
|---|---|---|
| `eligibleNamespaces` | `[]` | Static allowlist. When empty, the controller falls back to annotation-based lookup (`simian.chaos/eligible="true"`), which is the preferred mode for installations using `simian arena create`. |

## Provisioner subsystem (M2 Part A)

| Value | Default | Notes |
|---|---|---|
| `provisioner.enabled` | `true` | Ships the `simian-provisioner` SA + ClusterRole + ValidatingAdmissionPolicy backstop. Disable for installs where arenas are managed by an operator using their kubeconfig (no in-cluster provisioner). |

## LLM provider

| Value | Default | Notes |
|---|---|---|
| `llm.provider` | `gemini` | `gemini` or `stub`. |
| `llm.model` | `""` (default `gemini-2.5-pro`) | |
| `llm.vertex.enabled` | `true` | Vertex via Workload Identity (production-recommended). |
| `llm.vertex.project` | `gke-demos-345619` | Replace for your install. |
| `llm.vertex.location` | `us-central1` | |
| `llm.apiKey.enabled` | `false` | Alternative to Vertex; mounts a Kubernetes Secret. |
| `llm.apiKey.secretRef` / `secretKey` | `simian-llm` / `geminiApiKey` | |

## Executor safety policy

| Value | Default | Notes |
|---|---|---|
| `executor.durationCeiling` | `15m` | Hard cap per fault. Recommended overlay: `5m`. |
| `executor.permittedTiers` | `[namespace, node]` | Blast-radius tiers permitted. Recommended overlay: `[namespace]` (opt-in to node tier per install). |
| `executor.maxConcurrentFaults` | `0` (no cap) | Total leased faults across namespaces. Recommended overlay: `1`. |
| `executor.minCooldown` | `0s` | Per-namespace cooldown. Recommended overlay: `60s`. |
| `executor.recentFaultsCapacity` | `100` | Bounded ring backing the `get_recent_faults` MCP tool. |

## Topology + SUT

| Value | Default | Notes |
|---|---|---|
| `topology.resync` | `30s` | Informer resync interval. Recommended overlay: `60s` for prod (lower API server load). |
| `sutInController.enabled` | `false` | Required for `simian sut deploy --use-controller` (the in-controller SUT path). Recommended overlay: `true`. |
| `sutInjection.envoyFaults` | `false` | Whether to inject the Envoy fault sidecar into SUT Deployments. **Off by default** because the iptables interception breaks gRPC kubelet probes — see [Known limitations]({{< relref "known-limitations.md" >}}). Only enable for SUTs whose probes are HTTP-only or TCP-only. |

## Autonomous mode

| Value | Default | Notes |
|---|---|---|
| `autonomous.enabled` | `false` | When true, the controller runs the autonomous planning loop. |
| `autonomous.namespaces` | `[]` | Required when `enabled: true`. Arena namespaces the loop targets. |
| `autonomous.cycleInterval` | `5m` | Recommended overlay: `10m` (slower; more time to observe). |
| `autonomous.maxFaultsPerCycle` | `3` | Recommended overlay: `1` (one fault per cycle to start). |
| `autonomous.maxSeverityPerCycle` | `namespace` | Highest blast tier the loop will apply. |
| `autonomous.hypothesisHint` | `""` | Optional soft preference passed to the LLM. Use this to bias toward newer engines (network-policy, envoy-fault). |

## MCP server

| Value | Default | Notes |
|---|---|---|
| `mcp.port` | `8081` | |
| `mcp.serviceType` | `ClusterIP` | |

## Resources + security

| Value | Default | Notes |
|---|---|---|
| `resources.requests.cpu` / `.memory` | `100m` / `128Mi` | Recommended overlay: `200m` / `256Mi`. |
| `resources.limits.cpu` / `.memory` | `500m` / `512Mi` | Recommended overlay: `1000m` / `1Gi` (prevents OOM during LLM bursts). |
| `podSecurityContext` | restricted-PSS-compatible | `runAsNonRoot: true`, `runAsUser: 65532`, `seccompProfile.type: RuntimeDefault`. |
