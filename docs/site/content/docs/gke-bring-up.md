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

# Declarative state, no dataplane at all: a workload synthesized broken.
# Every field of the spec is optional; run each of the twelve kinds. NoOp is the
# control — it synthesizes a *healthy* workload, and its gate passing is what
# tells you a later empty finding means "nothing was wrong" and not "the probe
# never worked here".
for kind in ImageUnresolvable ContainerExitLoop MemoryLimitSqueeze Unschedulable \
            JobFailure SelectorDrift UnboundClaim DependencyStall \
            PDBGridlock CertExpiry NoOp; do
  bin/simian chaos --engine kube-state --kind "$kind" --api-version apps/v1 \
    --namespace simian-gke-1 --duration 4m
done

# RolloutStuck is held out of the loop because it needs a longer lease than the
# rest: Apply waits for a healthy revision to come up before wedging the next
# one, and the gate then waits out the Deployment's progress deadline.
bin/simian chaos --engine kube-state --kind RolloutStuck --api-version apps/v1 \
  --namespace simian-gke-1 --duration 10m
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
| `kube-state` `ImageUnresolvable` | gate passed in 13.1s | pods reached `ImagePullBackOff` after 7 polls |
| `kube-state` `ContainerExitLoop` | gate passed 4 runs of 4, 2.2s–4.4s | `lastState.terminated.reason: Error`, exit 1 |
| `kube-state` `MemoryLimitSqueeze` | gate passed in 4.4s | `lastState.terminated.reason: OOMKilled`, exit 137 |
| `kube-state` `Unschedulable` | gate passed in 2.2s | `PodScheduled=False`, reason `Unschedulable`; node count unchanged, no `TriggeredScaleUp` |
| `kube-state` `JobFailure` | gate passed in 37.0s over 18 polls | the Job's condition reached `BackoffLimitExceeded` after its retries ran out — the slowest gate in the set, because the backoff is the fault |
| `kube-state` `SelectorDrift` | both gates passed, 2.2s then 0.1s | pods `Ready=True`, and *then* the Service's EndpointSlices carried no addresses |
| `kube-state` `UnboundClaim` | both gates passed, 0.1s then 0.1s | claim `Pending`, and the pod that mounts it `Unschedulable` |
| `kube-state` `DependencyStall` | all three gates passed, 2.2s / 0.1s / 0.2s | pods `Ready=True`, EndpointSlice `conditions.ready` true, and then the log line found in `checkout-api-…-s7btf` |
| `kube-state` `NoOp` | gate passed in 2.2s | pods `Ready=True` — the control, and it is supposed to pass |
| `kube-state` `PDBGridlock` | both gates passed, 2.3s then 0.1s | pods `Ready=True`, budget reporting `disruptionsAllowed: 0` — and an eviction call against the pod returned `429 Cannot evict pod as it would violate the pod's disruption budget` |
| `kube-state` `CertExpiry` | both gates passed, 2.3s then 0.1s | pods `Ready=True`, `tls.crt` present in the mounted Secret; `openssl x509` read back `notAfter` exactly six hours out and `notBefore` ninety days back |
| `kube-state` `RolloutStuck` | both gates passed, 61.8s then 0.1s, twice within 0.05s of each other | `Progressing` reason `ProgressDeadlineExceeded` — the deployment's own 60s deadline, to the second — with the previous revision still `2/2 Running` and the new pod in `CrashLoopBackOff` |

The bundle rows were measured a day later, 2026-09-05/06, on the same cluster and
in a scratch namespace; everything above them came from the single run described
at the top. Efficacy rate across them was 1.00.

The multi-gate kinds are worth a second look. `SelectorDrift` and `UnboundClaim`
each prove their fault in two steps, in order, because the second step's evidence
is an *absence* — no endpoint addresses, no schedulable pod — and an absence on
its own is also what you get when nothing was created at all. Settle probes run
in sequence and stop at the first failure, so the first gate ("the workload is
Ready", "the claim exists and is Pending") is what makes the second one mean
something. See [efficacy probes]({{< relref "efficacy-probes.md" >}}#when-the-evidence-is-an-absence-something-else-has-to-prove-it-is-not-vacuous).

`DependencyStall` is the inverse case and takes three. Its first two gates assert
the workload is *healthy* — Ready pods, ready endpoints — and only then does the
third read the log. Without them a gate that just grepped the log would pass
against a crash-looping pod that printed the line on its way down; with them,
the finding means "and only the log is wrong", which is the whole point of the
kind. It is also the one kind where `kubectl get pods`, `kubectl get svc` and
`kubectl describe deploy` all report a healthy namespace.

`RolloutStuck` is the one that only a live cluster could have taught. The first
GKE run took the arena down: a container with no readiness probe is Ready for as
long as it is running, and a container that exits after 200ms is running for
200ms — long enough that the kubelet reported the broken pods Ready, the
Deployment controller declared the new ReplicaSet available and scaled the
working revision to zero. A completed rollout does not un-complete, so the
progress-deadline clock had already stopped when the pods began to crash, and the
gate correctly refused the fault: `ProgressDeadlineExceeded` never arrived. Adding
`minReadySeconds` fixed the outage but not the timing — the deadline now reset on
every restart's readiness flicker, so the condition first appeared at 159s and
then flipped back to `ReplicaSetUpdated`. The kind now ships with a readiness
probe on the broken revision that cannot pass, and the condition lands at 61.8s
and stays. None of this is visible against a fake clientset, where status is
whatever the test writes.

`NetworkChaos` landing on Dataplane V2 contradicts what this project documented for the last year. It is a real measurement, not a correction of a mistake: the bypass was verified at the time on an older Cilium. Treat it as version-dependent and re-check per cluster — see [known limitations]({{< relref "known-limitations.md" >}}#chaos-meshs-networkchaos-may-or-may-not-work-on-gke-dataplane-v2--measure-it). Reading the audit record is the check:

```bash
# passed: true means the probe saw the fault; the fault is live.
# passed: false means it was rolled back and nothing is running.
grep fault.efficacy controller.log | jq '.payload | {probe, passed, expected, observed}'
```

### `Unschedulable` and Node Auto-Provisioning

The `Unschedulable` kind defaults to a CPU request of `1000`, which looks absurd
until you consider what a *merely* large request does on GKE. 64 CPU is
unschedulable on today's nodes but perfectly **satisfiable by a bigger one**, so
the cluster autoscaler or Node Auto-Provisioning reads it as a provisioning
signal: it adds a node, the pod schedules, and the fault heals partway through
the experiment — with a machine on the bill. A request nothing can satisfy is
declared unschedulable and left alone. On the run above the node count stayed at
4 and no `TriggeredScaleUp` event was emitted.

If your scenario is about placement rather than capacity, use `node_selector`
instead; the two are mutually exclusive, and setting both would make the
`FailedScheduling` message name whichever predicate the scheduler checked first.

### `CrashLoopBackOff` is not a state you can poll for

The obvious gate for `ContainerExitLoop` is `state.waiting.reason ==
CrashLoopBackOff`. It passed in 6.5s on the first run here and then missed
entirely on the second — 44 polls over 91s, every one of them reading empty,
against a pod the event log showed was visibly backing off.

A container that exits immediately spends almost all of its time with the
*previous* termination showing in `state.terminated`; the kubelet flips to
`waiting: CrashLoopBackOff` only in a narrow window around each restart
decision. Polling the same pod by hand every 10s caught it once in six:

```
last=[Error] restarts=[4] wait=[]
last=[Error] restarts=[4] wait=[]
last=[Error] restarts=[4] wait=[]
last=[Error] restarts=[4] wait=[]
last=[Error] restarts=[4] wait=[CrashLoopBackOff]
last=[Error] restarts=[5] wait=[]
```

`lastState.terminated.reason` is stable from the first restart on, so that is
what the gate reads. Four consecutive runs against the same cluster passed in
2.2s, 4.4s, 2.2s and 2.3s — two or three polls each, no spread worth the name.
If you write your own probe against a restarting workload, read `lastState`, not
`state`.

### What the `MemoryLimitSqueeze` shakedown found

The first implementation wrote into a `medium: Memory` emptyDir, on the correct
theory that tmpfs pages are charged to the writing container's cgroup. On GKE
that produced `StartError`, not `OOMKilled`, at every limit tried — because a
tmpfs emptyDir belongs to the **pod**, not the container. Its pages outlive the
OOM kill, so the restarted container's `runc init` is killed against a cgroup
that is already full (`container init was OOM-killed (memory limit too low?)`)
before any of the workload's own code runs.

The gate caught it: `probe "simian-oom-killed" never passed in 2m0s (57 polls):
wanted "OOMKilled" in output, last saw "StartError"`, and the fault was rolled
back rather than reported as applied. The kind now allocates anonymous memory,
which is freed with the process, so every restart cycle reproduces the same
clean `OOMKilled`. This is the failure mode the whole efficacy story exists for
— without the gate it would have shipped as a fault that "worked".

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
