---
title: "GKE bring-up"
linkTitle: "GKE bring-up"
weight: 15
description: "Pointing Simian at a real GKE cluster, and finding out which faults actually land on it."
---

[Getting started]({{< relref "getting-started.md" >}}) gets you a fault on whatever cluster your kubeconfig names. This page is about the first run against a real GKE cluster: what to check before you start, what changes on Dataplane V2, and how to establish — by measurement, not by assumption — which fault kinds land there.

Every command below was run on 2026-09-04 against a Standard GKE cluster in `us-central1`:

```
GKE          1.36.3-gke.1537000 (RAPID)
dataplane    ADVANCED_DATAPATH (Dataplane V2 — Cilium v1.19.4-gke.49 via anetd)
nodes        4 × COS, kernel 6.12.94+, containerd 2.2.5
chaos-mesh   v2.8.2
```

The numbers quoted are the ones that run produced. Yours will differ; the point is the shape of the check, not the figure.

## Before you start

```bash
# Dataplane: ADVANCED_DATAPATH means Dataplane V2. Anything else is kube-proxy
# + your chosen CNI, and Chaos Mesh behaves the way its own docs describe.
gcloud container clusters describe "$CLUSTER" --region "$REGION" \
  --format="value(networkConfig.datapathProvider,autopilot.enabled)"

# Chaos Mesh, with chaos-daemon on every node you plan to inject into.
kubectl -n chaos-mesh get pods
kubectl get crd -o name | grep chaos-mesh.org | wc -l
```

Two GKE-specific constraints worth knowing before you spend cluster time:

- **Autopilot will not run Chaos Mesh's `chaos-daemon`.** It needs a privileged pod with host PID and the container runtime socket. On Autopilot, use the `network-policy` and `envoy-fault` engines, both of which are ordinary workloads and API calls.
- **Node-tier faults do more than you think on a managed cluster.** `KernelChaos` and `PhysicalMachineChaos` act on the node, and a GKE node pool with autorepair will react to what you did to it. Start with `--permitted-tiers namespace`, which is the fence that keeps the executor from applying them at all:

  ```bash
  bin/simian serve --permitted-tiers namespace
  ```

  An unrecognised tier name stops the controller starting rather than falling back to the default, so a typo here fails loudly. See [Helm values]({{< relref "helm-values.md" >}}#how-permittedtiers-fails).

## Run the controller in-cluster, not on your laptop

Local `simian serve` against a remote cluster works — arena creation, fault apply and clear are all API calls — but the [efficacy probes]({{< relref "efficacy-probes.md" >}}) dial pod IPs directly. Off-cluster, that measures your workstation's round trip to the VPC instead of an in-cluster one.

On the cluster above, the same `frontend` pod answered in ~40ms from inside the cluster and 170–330ms from a workstation. The SOT half of the delay gate requires the target to be *faster* than a quarter of the injected latency before it will accept the fault as gateable, so an off-cluster controller fails prechecks that an in-cluster one passes, on a fault that is perfectly fine.

Use local serve to get your first fault out. Move to [the Helm chart]({{< relref "deploy.md" >}}) before you trust the verdicts.

## Arena and SUT

```bash
bin/simian sut deploy --namespace simian-gke-1 --create-arena --use-controller
```

1m43s on a warm 4-node cluster: namespace, RBAC, twelve Online Boutique deployments, and a 30s steady-state baseline window. `--use-controller` routes the deploy through the running controller's `establish_baseline` tool so its `get_baseline` cache is populated — without it the baseline is captured client-side and an agent asking the controller for it gets nothing. Envoy injection is off by default and should stay off for Online Boutique — its gRPC kubelet probes do not survive the current interception model ([known limitations]({{< relref "known-limitations.md" >}}#envoy-injection-breaks-grpc-kubelet-probes)).

## Find out what actually lands

This is the part that matters on GKE, and the reason the efficacy gates exist. Apply one fault per engine and read the `fault.efficacy` record rather than the exit code.

```bash
# Chaos Mesh, dataplane-independent: a pod kill is self-evident, so it has no gate.
bin/simian chaos --kind PodChaos --namespace simian-gke-1 --workload recommendationservice \
  --duration 60s \
  --spec '{"action":"pod-kill","mode":"one","selector":{"labelSelectors":{"app":"recommendationservice"}}}'

# Chaos Mesh, dataplane-dependent: this is the one to be suspicious of on DPv2.
bin/simian chaos --kind NetworkChaos --namespace simian-gke-1 --workload frontend \
  --duration 2m \
  --spec '{"action":"delay","mode":"all","selector":{"labelSelectors":{"app":"frontend"}},"delay":{"latency":"250ms","correlation":"0","jitter":"0ms"}}'

# Node-resource pressure: no dataplane involvement at all.
bin/simian chaos --kind StressChaos --namespace simian-gke-1 --workload productcatalogservice \
  --duration 90s \
  --spec '{"mode":"one","selector":{"labelSelectors":{"app":"productcatalogservice"}},"stressors":{"cpu":{"workers":2,"load":80}}}'

# Dataplane-independent partition: a plain NetworkPolicy, enforced by Cilium.
bin/simian chaos --engine network-policy --kind NetworkPolicy \
  --api-version networking.k8s.io/v1 \
  --namespace simian-gke-1 --workload cartservice --duration 90s \
  --spec '{"labelSelectors":{"app":"cartservice"},"directions":["ingress","egress"]}'
```

What the run above produced:

| Fault | Result | Evidence |
|---|---|---|
| `PodChaos` `pod-kill` | applied in 0.8s | the pod's name changed |
| `StressChaos` CPU, 2 workers @ 80% | applied, injected | `kubectl top pod` went 12m → 301m (the container's CPU limit is the ceiling, not the stressor's ask) |
| `NetworkChaos` `delay` 250ms | gate passed in 3.2s | in-cluster request went 90ms → 3.9s, back to 90ms after clear |
| `NetworkChaos` `partition` | landed | HTTP 200 → connect timeout → HTTP 200 |
| `network-policy` partition | gate passed in 5.2s | SOT saw the target answer, Settle saw the connection time out |
| `envoy-fault` | not exercised | needs Envoy injection, which Online Boutique's gRPC probes will not tolerate |

`NetworkChaos` landing on Dataplane V2 contradicts what this project documented for the last year. It is a real measurement, not a correction of a mistake: the bypass was verified at the time on an older Cilium. Treat it as version-dependent and re-check per cluster — see [known limitations]({{< relref "known-limitations.md" >}}#chaos-meshs-networkchaos-may-or-may-not-work-on-gke-dataplane-v2--measure-it). Reading the audit record is the check:

```bash
# passed: true means the probe saw the fault; the fault is live.
# passed: false means it was rolled back and nothing is running.
grep fault.efficacy controller.log | jq '.payload | {probe, passed, expected, observed}'
```

## Sizing a delay so the gate can see it

The delay gate is deliberately conservative: SOT demands the target answer in under a quarter of the injected latency, and Settle demands at least half of it. That is a 4× signal-to-noise requirement, and it is what stops "the app was always slow" from being reported as a fault that landed.

Online Boutique's `frontend` answers in 40–240ms depending on what its downstreams are doing. A 250ms delay against it is right at the edge — the SOT threshold is 62.5ms, so the precheck passes or fails on which sample it happens to take. Injecting 2s instead puts the SOT threshold at 500ms, clear of the noise.

Pick the injected latency relative to the workload's own baseline, not to the number that sounds dramatic.

## Cleaning up

```bash
bin/simian chaos --list-active
bin/simian chaos --clear f-<uid>
bin/simian sut destroy --namespace simian-gke-1 --with-arena
```

`sut destroy` refuses while Simian-managed faults are still leased. Leases also expire on their own, and the reaper sweeps every 30s by default, so a resource can outlive its deadline by up to one sweep.
