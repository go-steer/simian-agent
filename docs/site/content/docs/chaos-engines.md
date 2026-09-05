---
title: "Using the chaos engines"
linkTitle: "Chaos engines"
weight: 70
description: "Directed and autonomous patterns for chaos-mesh, network-policy, envoy-fault, and kube-state."
---

Simian ships four chaos engines. Each is a `simian.ChaosDriver` registered with the executor; the LLM sees all of them via the catalog mechanism and can pick whichever fits the plan.

The first three perturb a *running dataplane* — traffic, processes, resources. The fourth produces the other half of the failure space: an object that is simply wrong in the API server, which is where an SRE agent spends most of its time.

| Engine | What it does | When to use it |
|---|---|---|
| `chaos-mesh` | The full Chaos Mesh CRD catalog: PodChaos, StressChaos, IOChaos, TimeChaos, NetworkChaos, etc. | Default for everything. Whether NetworkChaos lands on GKE Dataplane V2 depends on the Cilium version — it does on current GKE, and its efficacy gate tells you either way. See [Known limitations]({{< relref "known-limitations.md" >}}). |
| `network-policy` | Standard `networking.k8s.io/v1` NetworkPolicy partitions (deny ingress / egress / both). | Network partition chaos on any cluster where NetworkChaos isn't reliable. Partition only — no delay / loss / jitter. |
| `envoy-fault` | HTTP-layer delay + abort via an injected Envoy sidecar. Two kinds: `EnvoyHttpDelay`, `EnvoyHttpAbort`. | HTTP/gRPC delay or error injection on DPv2. Requires the SUT to be deployed with `--no-envoy-faults=false` (off by default — see [Known limitations]({{< relref "known-limitations.md" >}}#envoy-injection-breaks-grpc-kubelet-probes)). |
| `kube-state` | Declarative-state faults: synthesizes a bundle of objects that is born broken. Nine kinds: `ImageUnresolvable`, `ContainerExitLoop`, `MemoryLimitSqueeze`, `Unschedulable`, `JobFailure`, `SelectorDrift`, `UnboundClaim`, `DependencyStall`, and `NoOp` (the control). | Wedged rollouts, bad image references, crash loops, OOM kills, unschedulable pods, failed batch jobs, endpointless Services, claims that never bind and a workload whose only symptom is in its own log — states rather than events, and the ones an SRE agent triages most. Works on any cluster; needs no Chaos Mesh and no sidecar. |

## Directed-control patterns

All four engines accept the same `simian chaos --engine ... --kind ... --spec '<inline JSON>'` shape:

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

# kube-state: synthesize a workload stuck in ImagePullBackOff for 5 minutes.
# Every field of the spec is optional — '{}' produces the failure state.
simian chaos --engine kube-state \
  --kind ImageUnresolvable --api-version apps/v1 \
  --namespace boutique-1 --duration 5m

# kube-state: a pod nothing can schedule
simian chaos --engine kube-state \
  --kind Unschedulable --api-version apps/v1 \
  --namespace boutique-1 --duration 5m \
  --spec '{"node_selector":{"failure-domain.example.com/zone":"nowhere"}}'

# kube-state: a Service in front of nothing. Every pod Running and Ready, the
# traffic going nowhere — the shape that catches an agent grading `get pods`.
simian chaos --engine kube-state \
  --kind SelectorDrift --api-version apps/v1 \
  --namespace boutique-1 --duration 5m

# kube-state: a workload that serves fine and cannot reach what it calls.
# Nothing is wrong on any object — Deployment Available, pods Ready against a
# real HTTP probe, Service endpointed. The only evidence is the log line, which
# is what the `logs` probe gates on and what a subject has to have read.
simian chaos --engine kube-state \
  --kind DependencyStall --api-version apps/v1 \
  --namespace boutique-1 --duration 5m \
  --spec '{"message":"level=error msg=\"upstream request failed\" upstream=payments-api err=\"context deadline exceeded after 30s\""}'

# kube-state: the control. Synthesizes a healthy workload, on purpose.
simian chaos --engine kube-state \
  --kind NoOp --api-version apps/v1 \
  --namespace boutique-1 --duration 5m
```

`kube-state` targets a namespace, not a workload: it creates its own. `--workload`
is ignored, and nothing that was already running is touched — which is what makes
a baseline captured before the fault still comparable afterwards.

Give these faults **at least 3m**. Apply does not return until the efficacy probe
has seen the failure state, that wait comes out of the fault's own lease, and a
backoff state can take 30s or more to appear.

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

## How a fault recovers if the controller dies

Chaos Mesh resources carry a `spec.duration` that `chaos-controller-manager`
honours server-side, so a `chaos-mesh` fault recovers on its own even if Simian
is killed mid-fault.

A `network-policy` or `kube-state` fault has no such backstop — a NetworkPolicy
or a synthesized Deployment stays until something deletes it, and the in-memory
lease that was going to delete it dies with the process. So those drivers write
the deadline onto the object itself:

```
metadata:
  labels:
    simian.chaos/managed: "true"
    simian.chaos/fault-uid: f-01M0E6...
  annotations:
    simian.chaos/expires-at: "2026-08-19T23:42:53Z"   # RFC3339, UTC
```

On startup and on every reap tick, the controller sweeps the eligible
namespaces for `simian.chaos/managed=true` objects whose `expires-at` has
passed and deletes them, emitting `lease.expired` with
`reason: orphan-reaped`. A restarted controller therefore clears partitions the
previous one leaked, without needing to have known about them.

Two deliberate non-behaviours:

- An object with **no** `expires-at`, or an unparseable one, is never deleted.
  Simian will not remove something it cannot prove has expired — it may be an
  operator's own. It still shows up in `simian arena describe`.
- The scan only looks in namespaces that are declared arenas — the
  `--eligible-namespace` allowlist, or every namespace annotated
  `simian.chaos/eligible: "true"` when no allowlist is given. That set is
  resolved on each sweep, so an arena created after the controller started is
  swept without a restart. If nothing has opted in, the scan covers nothing —
  not everything.

`simian arena destroy` refuses while any Simian-managed fault is live, and names
each one so you know what to clear:

```
error: arena: 1 simian-managed chaos resource(s) still active in "boutique-1"
       (NetworkPolicy/simian-np-01m0e6...); clear them first or pass --force
```

## Background reading

- [DPv2-compatible chaos engines]({{< relref "dpv2-chaos-engines.md" >}}) — full design rationale for `network-policy` and `envoy-fault`.
- [Efficacy probes]({{< relref "efficacy-probes.md" >}}) — the default gates, including all eight `kube-state` ones.
- [GKE bring-up]({{< relref "gke-bring-up.md" >}}) — measuring which of these engines actually land on your own GKE cluster.
- [Known limitations]({{< relref "known-limitations.md" >}}) — the GKE DPv2 NetworkChaos question and the Envoy injection / gRPC probe interaction.
