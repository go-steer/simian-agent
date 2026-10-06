---
title: "Helm values reference"
linkTitle: "Helm values"
weight: 45
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
| `llm.vertex.project` | `""` | **Required** while `llm.vertex.enabled`: the GCP project whose Vertex AI the controller calls. The chart refuses to render without it. |
| `llm.vertex.location` | `us-central1` | |
| `llm.apiKey.enabled` | `false` | Alternative to Vertex; mounts a Kubernetes Secret. |
| `llm.apiKey.secretRef` / `secretKey` | `simian-llm` / `geminiApiKey` | |

## Executor safety policy

| Value | Default | Notes |
|---|---|---|
| `executor.durationCeiling` | `15m` | Hard cap per fault. Recommended overlay: `5m`. |
| `executor.permittedTiers` | `[namespace, node]` | Blast-radius tiers permitted: `namespace`, `node`, `external`. Renders one `--permitted-tiers=` arg per entry. Recommended overlay: `[namespace]` (opt-in to node tier per install). |
| `executor.maxConcurrentFaults` | `0` (no cap) | Total leased faults across namespaces. Recommended overlay: `1`. |
| `executor.minCooldown` | `0s` | Per-namespace cooldown. Recommended overlay: `60s`. |
| `executor.recentFaultsCapacity` | `100` | Bounded ring backing the `get_recent_faults` MCP tool. |

### How `permittedTiers` fails

This is the one value an operator sets to make the policy *narrower* — usually
to keep node-level chaos off a cluster they care about — so its failure modes
are deliberately loud rather than convenient.

- **An unrecognised tier name stops the controller starting**, with
  `--permitted-tiers: unknown blast-radius tier "..."`. A typo that fell back
  to the default would silently restore the very tier the operator was
  removing.
- **An empty list means "use the built-in default"** (`namespace`, `node`), not
  "permit nothing". The chart renders no flag at all in that case, and a flag
  the operator never set must not be a capability change.
- **`--set executor.permittedTiers={}` is not an empty list.** Helm's `--set`
  turns it into a one-element list holding the empty string, which renders
  `--permitted-tiers=` and stops the controller with an unknown-tier error. Use
  `--set-json 'executor.permittedTiers=[]'`, or set it in a values file.

`autonomous.maxSeverityPerCycle` is validated the same way, at startup rather
than in the loop: an unparseable cap makes every planned step ineligible, which
otherwise looks exactly like a planner that produced nothing.

## Topology + SUT

| Value | Default | Notes |
|---|---|---|
| `topology.resync` | `30s` | Informer resync interval. Recommended overlay: `60s` for prod (lower API server load). |
| `sutInController.enabled` | `false` | Required for `simian sut deploy --use-controller` (the in-controller SUT path). Recommended overlay: `true`. Covers only the namespaces in `eligibleNamespaces`; for arenas made with `simian arena create`, pass `--sut-in-controller` there (or `sut deploy --create-arena --use-controller`, which does it for you). |
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

## Audit trail

| Value | Default | Notes |
|---|---|---|
| `audit.file.enabled` | `true` | Also append audit events to a JSON-lines file (`--audit-file`). Stdout logging is unaffected. |
| `audit.file.path` | `/var/lib/simian/audit.jsonl` | Its directory is the mount point for the audit volume. |
| `audit.file.maxBytes` | `104857600` | Past this the file rotates to `<path>.1`; two generations are kept. |
| `audit.file.persistence.enabled` | `true` | Keep the file on a PVC (`simian-audit`, kept on uninstall) rather than an emptyDir, and switch the Deployment to `strategy: Recreate`. Needs a default StorageClass, or `storageClass` below. Default since chart 0.1.10. |
| `audit.file.persistence.size` | `1Gi` | |
| `audit.file.persistence.storageClass` | `""` | Empty uses the cluster default. |

With persistence off, the file is on an emptyDir, which survives container
restarts but not the pod being deleted or rescheduled; persistence keeps the
trail past both. At start-up the controller
reads the file for faults the previous process applied and never ended. A fault
still before its deadline is adopted (`lease.adopted`, reason
`untracked-after-restart`): it counts against `--max-concurrent-faults` and the
health gate, and is cleared at its deadline as if this process had applied it.
Every other one gets a `lease.expired` event with the same reason, so a restart
no longer leaves faults with no end on record. It then lists the faults still
running in the arenas from the objects the drivers left (`lease.adopted`,
`found_in: cluster`) and adopts any the file did not cover — all of them when
the file was lost with an emptyDir. A fault with no object of its own
(`envoy-fault`) can only be adopted from the file. Read
the trail back with [`simian audit export`]({{< relref "cli-reference.md#exporting-the-audit-trail" >}}).

On GKE the controller's stdout already reaches Cloud Logging, so the audit
events are retained there as well; the file is what you have on clusters
without a log pipeline, and what `simian audit export` reads directly.

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
| `podSecurityContext` | restricted-PSS-compatible | `runAsNonRoot: true`, `runAsUser: 65532`, `fsGroup: 65532` (so the controller can write a PVC-backed audit volume), `seccompProfile.type: RuntimeDefault`. |
