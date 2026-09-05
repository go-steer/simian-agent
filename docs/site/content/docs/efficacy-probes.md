---
title: "Efficacy probes"
linkTitle: "Efficacy probes"
weight: 75
description: "Probes gate Apply so a fault that silently did nothing is never reported as applied."
---

A fault that the cluster accepts and then quietly does nothing is the worst
outcome Simian can produce. It is not a missing measurement — it is a wrong
one. The eval result reads *"the agent missed a network partition"* when there
was no partition, and it averages in with the real data points.

Probes close that gap. A fault carrying them is not reported as applied until
Simian has *observed* it land.

Every fault kind Simian can inject dataplane-side [carries one by
default](#default-gates) — the operator does not have to remember, and the
planner cannot forget.

## Efficacy, not outcome

Simian checks that the fault **landed**. It does not check what the fault
**caused**.

* **Efficacy** — is the pod genuinely in `CrashLoopBackOff`? Does the
  NetworkPolicy exist and select the right pods? Simian's business.
* **Outcome** — did latency rise, did the SLO burn, did the agent notice? The
  agent under test's business, and what it is scored on.

Measuring outcome here would let the harness grade its own experiment, so the
probe types are deliberately limited to things that answer "did it land".

## Two phases: SOT, then Settle

Probes hang off `FaultManifest.probes` and carry a `mode`. Simian schedules two
of them; the other Litmus modes round-trip untouched.

| Mode | When it runs | What a failure means |
|---|---|---|
| `SOT` | Before `driver.Apply` — nothing has touched the cluster | The starting conditions do not hold. The manifest is **rejected**; there is nothing to roll back |
| `Settle` | After `driver.Apply` | The fault was accepted and did not land. The fault is **backed out** |

The SOT phase exists because half the interesting proofs are differential. "The
target does not answer" is not evidence of a partition against a workload that
was not answering in the first place — it is the same vacuous pass as
`"expect_contains": ""`, one layer up. So an unreachability gate is only
allowed to mean anything if a reachability check passed first, and Simian
attaches the pair together.

Running SOT *before* Apply rather than after is deliberate: a fault whose
preconditions are already broken should never have been injected, and rejecting
it costs the arena nothing.

## Writing a probe

```json
{
  "engine": "chaos-mesh",
  "api_version": "chaos-mesh.org/v1alpha1",
  "resource_kind": "PodChaos",
  "duration": "2m",
  "spec": {"action": "pod-failure", "mode": "one",
           "selector": {"labelSelectors": {"app": "payments"}}},
  "targets": [{"namespace": "boutique", "name": "payments"}],
  "probes": [{
    "name": "payments crash-loops",
    "type": "k8s",
    "mode": "Settle",
    "spec": {
      "resource": "pods",
      "jsonpath": "{.items[*].status.containerStatuses[*].state.waiting.reason}",
      "expect_contains": "CrashLoopBackOff",
      "timeout": "90s",
      "interval": "2s"
    }
  }]
}
```

Submit it with `simian chaos --manifest ./fault.json`, or through the
`submit_manifest` MCP tool.

### The `k8s` probe type

`kubectl get <resource> -n <namespace> -o jsonpath=<expr>` in a loop. That
correspondence is deliberate: settle conditions already written for `kubectl`
port over without translation.

| Field | Required | Meaning |
|---|---|---|
| `resource` | yes | `pods`, `deployments.apps`, `endpointslices` — anything the cluster knows |
| `jsonpath` | yes | Evaluated exactly as `kubectl -o jsonpath` would, missing keys included |
| `expect_contains` | one of | Substring that must appear in the output |
| `expect_empty` | one of | Require *blank* output instead |
| `namespace` | no | Defaults to the fault's own target namespace |
| `name` | no | Read one named object instead of listing |
| `label_selector` | no | Narrow the list; mutually exclusive with `name` |
| `timeout` | no | Duration string, default `90s` |
| `interval` | no | Duration string, default `2s` |

When no `name` is given the probe lists, and the jsonpath sees the whole list
object — so `{.items[*]...}` works just as it does with `kubectl`.

`expect_empty` exists because some faults have no string to match on. A Service
with no ready endpoints is an *absence*, and `"expect_contains": ""` would look
like a check while passing unconditionally. Simian rejects a probe that
declares neither condition rather than accepting one that cannot fail.

### The `http` probe type

The `k8s` probe reads a field. Dataplane faults do not have one: a partition, a
netem delay and an injected 503 leave nothing on any Kubernetes object to
match. Chaos Mesh will happily report a `NetworkChaos` as `Injected` on a
cluster whose datapath never traversed the qdisc it installed.

So the `http` probe dials the target pods directly — pod IP, no Service, no
ingress — and asserts on what comes back, or on the fact that nothing does.

| Field | Meaning |
|---|---|
| `namespace` | Defaults to the fault's own target namespace |
| `label_selector` | Which pods to dial; defaults to the fault's own target labels |
| `name` | Dial one named pod instead |
| `port` | Defaults to the pod's first declared container port |
| `path` / `scheme` / `method` | Default `/`, `http`, `GET` |
| `jsonpath` | Evaluate the expression over a JSON body and match on the result instead of the raw body |
| `expect_reachable` | The connection must succeed, whatever the status |
| `expect_unreachable` | The connection must fail |
| `expect_status` | Exact status code |
| `expect_contains` | Substring of the body (or of the jsonpath rendering) |
| `expect_equals` | Whole trimmed value equals this |
| `min_latency` / `max_latency` | Bound the observed round trip |
| `request_timeout` | Per-request deadline, default `3s` |
| `timeout` / `interval` | Poll budget and gap, default `90s` / `2s` |

Every pod the selector resolves must satisfy every stated expectation, and a
round that resolves *no* pods is a failure rather than a pass — "nothing to
dial" is the same vacuous success `expect_empty` exists to refuse.
`expect_unreachable` cannot be combined with any other expectation, because
there is no response to assert on.

Two details are load-bearing and worth knowing about:

* **Every attempt dials a fresh connection.** A partition drops *new* flows;
  conntrack lets an already-established one through. A probe that reused the
  socket its SOT check opened would go on getting `200`s in 1ms through a
  partition that really did land, and reject a working fault. This was not
  theoretical — it is exactly what the first live run did.
* **Latency is measured through the body, not to the first byte.** A delay
  fault can hold the body rather than the headers.

#### A request that never came back is slow

A `min_latency` gate has to decide what a request that timed out means. The
literal reading — no response, no measurement, no pass — is wrong here, and
wrong in the expensive direction: it takes a delay fault that landed *harder*
than asked and reports it as a fault that did nothing. The SUT ate the chaos
and the audit record says it did not.

That is not hypothetical. An injected 250ms delay on Online Boutique's
`frontend` produced a 3.9s page load, because one page fans out into a dozen
internal round trips and each pays the delay twice. Any per-request deadline
sized against the *injected* number will expire.

So a timeout satisfies `min_latency`, under four conditions, all of which have
to hold:

* `min_latency` is the *only* expectation on the probe. A status code, a body
  match or a reachability check cannot be satisfied by a response that never
  arrived, and a timeout fails them.
* The request ran at least `min_latency` before giving up. A connection refused
  in 2ms is not slowness, it is a dead pod.
* The failure is genuinely a timeout — `context.DeadlineExceeded` or a
  `net.Error` reporting `Timeout()`. A connection reset at 120ms is a broken
  target, not a slow one, and is not counted.
* `request_timeout >= min_latency`, so the deadline could not have fired before
  the threshold was reachable.

The caller's own deadline is not evidence either: a cancelled probe fails
rather than passing on the way out.

The SOT half is what makes the inference safe. `simian-fast-before` has already
proved this pod answers well inside the threshold, so "it stopped answering
within the deadline" is a change Simian caused, not a property of the workload.
The `Expected` string says so out loud — `latency >= 125ms (or no response
within 1s)` — so the audit record never claims a measurement it does not have.

### Why not `cmd`

An `exec`-into-the-pod probe would read `tc -s qdisc` and settle a whole class
of question directly. It is deliberately not implemented: it needs `pods/exec`
on the chaos controller's ServiceAccount, which is a real blast-radius increase
and deserves its own decision rather than arriving as a side effect of a probe
type. Everything the default gates need is reachable over HTTP.

## Default gates

Probes only help if they are present, and the component that writes the
manifest is the component being evaluated. A planner that can omit its own gate
will eventually omit it. So the gates are **not a manifest concern**: the
executor attaches them, per fault kind, from a table the manifest does not get
a vote on.

| Engine | Kind | Gate |
|---|---|---|
| `network-policy` | `NetworkPolicy` | Reachable before, unreachable after |
| `chaos-mesh` | `NetworkChaos` (`partition`) | Reachable before, unreachable after |
| `chaos-mesh` | `NetworkChaos` (`delay`) | Fast before, measurably slower after |
| `envoy-fault` | `EnvoyHttpDelay` | Admin API reports the delay runtime key at the requested percentage |
| `envoy-fault` | `EnvoyHttpAbort` | Admin API reports the abort runtime key at the requested percentage |
| `kube-state` | `ImageUnresolvable` | Pods reach `ImagePullBackOff` |
| `kube-state` | `ContainerExitLoop` | A container's `lastState.terminated.reason` is `Error` (non-zero exit) |
| `kube-state` | `MemoryLimitSqueeze` | A container's `lastState.terminated.reason` is `OOMKilled` |
| `kube-state` | `Unschedulable` | A pod condition carries reason `Unschedulable` |
| `kube-state` | `JobFailure` | The Job carries a condition of reason `BackoffLimitExceeded` |
| `kube-state` | `SelectorDrift` | Pods are Ready **and** the Service's EndpointSlices carry no addresses |
| `kube-state` | `UnboundClaim` | The claim is `Pending` **and** the pod mounting it reports `Unschedulable` |
| `kube-state` | `NoOp` | Pods are Ready — the control's gate is the one every other kind fails |

The table is keyed by `(engine, kind)`, not by cluster. The Chaos Mesh catalog
is derived from live CRD discovery, so anything hand-listed per installation
would go stale the moment a cluster shipped a different set of CRDs. The gate
attaches by kind, which means a CRD Simian has never seen on this cluster
before still arrives gated. Each entry's description also rides along on
the catalog as `efficacy_gate`, so a planner can tell a verified fault kind from
an unverified one before it picks.

A manifest overrides a default only by **naming it** — declaring a probe called
`simian-partitioned` replaces that one and leaves the rest. Everything else is
additive, so a manifest cannot dissolve its gate by declaring something
unrelated. The names are reserved and prefixed: `simian-reachable-before`,
`simian-partitioned`, `simian-fast-before`, `simian-delayed`,
`simian-envoy-runtime`, `simian-image-pull-failed`, `simian-crash-looping`,
`simian-oom-killed`, `simian-unschedulable`, `simian-job-failed`,
`simian-workload-ready`, `simian-no-endpoints`, `simian-claim-pending`.

### A synthesized fault has no SOT half, and that is not an oversight

Every `kube-state` gate is Settle-only. Every dataplane gate above needs a
precheck because its Settle assertion is *differential*: "the target does not
answer" proves nothing about a workload that was not answering beforehand.

A synthesized workload did not exist before `Apply`. "These pods are in
`ImagePullBackOff`" cannot be a pre-existing condition, because there was no
pre-existing anything — there is nothing for a precheck to rule out. When the
engine's `mutate` mode lands, which patches a workload that was already running,
the SOT half comes back with it.

That the gate can name pods that do not exist yet is why the synthesized
workload's name is derived from the fault UID rather than from a fresh random
value: the executor builds the probe *before* it calls `Apply`, so both sides
have to compute the same name from the manifest alone.

Each gate asserts the narrowest *stable* field it can, and the second word is
the one that cost a debugging session. The obvious gate for `ContainerExitLoop`
is `state.waiting.reason == CrashLoopBackOff`, and it is a coin flip: a
container that exits immediately spends almost all of its time with the
previous termination showing in `state.terminated`, and the kubelet flips to
`waiting: CrashLoopBackOff` only in a narrow window around each restart
decision. Measured on GKE 1.36 that window caught one poll in six — one run
passed in 6.5s, the next missed it across 44 polls and rolled back a fault that
had visibly landed. Both crash-loop kinds now read
`lastState.terminated.reason`, which is stable from the first restart on:
`Error` for a non-zero exit, `OOMKilled` for a memory kill.

`ImageUnresolvable` is the exception that may read `state.waiting.reason`: its
container never starts, so there is no restart cycle to race against and the
pod stays in `ImagePullBackOff`. `Unschedulable` reads the `PodScheduled`
condition's reason rather than `phase == Pending`, which would also pass while
an image is still pulling.

### When the evidence is an absence, something else has to prove it is not vacuous

`SelectorDrift` breaks a Service by pointing it past its own pods, so the state
that proves the fault landed is *no endpoint addresses* — and an empty read is
also what a namespace where nothing has been created yet produces. Gated on that
alone, the probe would pass in the moment before the workload existed and report
a fault that never landed as verified. That is the exact failure the gates exist
to prevent, arriving through the gate itself.

So the kinds whose evidence is an absence get two probes, and Settle probes run
**in order**, stopping at the first that does not pass. `simian-workload-ready`
runs first and only passes once the pods report `Ready`; `simian-no-endpoints`
runs second, against a namespace the first probe has just proved is populated.
A test in `pkg/catalog` refuses any gate that puts an `expect_empty` probe
first.

The window between the two is narrow enough to measure: on GKE 1.36 a Service
whose selector *does* match has its addresses published by the time
`kubectl wait --for=condition=Ready` returns, and the second probe polls ~100ms
after the first passes.

`UnboundClaim` is paired for a different reason. The pod it blocks reports
`Unschedulable`, which the scheduler also writes for taints, node selectors and
genuine resource shortage — so the gate reads the claim's own `Pending` phase
first. The cause before the symptom.

`NoOp`, the control, is gated on its workload being **healthy**. A control needs
a gate as much as a fault does: without one it would "inject" successfully
against a cluster too broken to run anything, and the subject's correct report
of nothing wrong would be scored as a correct answer rather than as the vacuous
pass it is.

Defaults are on unless the operator turns them off:

```
simian serve --default-efficacy-probes=false
```

### Size a delay against the workload, not against the drama

The delay gate is a 4× signal-to-noise requirement in disguise. SOT demands the
target answer in under `latency/4`; Settle demands at least `latency/2`. That
ratio is what stops "the app was always slow" from passing as "the fault
landed", and it means the injected number has to be chosen relative to the
target's own baseline.

Online Boutique's `frontend` answers in 40–240ms depending on what its
downstreams are doing, so a 250ms delay puts the SOT threshold at 62.5ms —
inside the noise, and the precheck passes or fails on which sample it happens
to take. Injecting 2s moves the threshold to 500ms, clear of it. A precheck
that keeps failing on a fault you believe in usually means the delay is too
small for the workload, not that the gate is broken.

### The envoy gate reads the value back

`envoy-fault`'s `Apply` is a `POST` to the sidecar's `/runtime_modify`. A `200`
from that endpoint says the request was accepted; it does not say the filter is
live. The gate does `GET /runtime` on the admin port and asserts on the value
Envoy reports it is actually running with:

```
jsonpath: {.entries['fault\.http\.delay\.fixed_delay_percent'].final_value}
expect_equals: "100"
```

which is the difference between "we asked" and "it happened". After
`clear_fault`, the same key reads `"0"`.

### What is deliberately left ungated

A gate that fires on a working fault is worse than no gate: it teaches the
operator to disable gates. Each of these has no default probe, on purpose.

| Case | Why |
|---|---|
| `loss`, `duplicate`, `corrupt`, `bandwidth` | Statistical. One request from one prober cannot separate "5% loss landed" from "5% loss did not" |
| Egress-only partitions | The controller proves an ingress cut by failing to reach the target. It has no vantage point from which to watch the target's own egress |
| `NetworkChaos` with `target` / `externalTargets` | The fault applies between two labelled sets and the controller is in neither |
| Delays under 100ms | Inside the noise of an in-cluster round trip |

Ungated is not *silently* ungated. The `executor.validated` event lists the
probes Simian attached under `default_probes`; no such key means the fault ran
unverified, which is a fact about the data point rather than a footnote.

## What happens on failure

`Apply` returns a typed `*simian.ExecutorError` with stage `probe` and reason
`probe-failed`, naming the probe and quoting what it last saw:

```
executor[probe:probe-failed]: probe "payments crash-loops" (k8s) never passed
in 1m30s (45 polls): wanted "CrashLoopBackOff" in output, last saw "Running"
```

That is deliberately distinguishable from `driver-failed`. A driver failure
means the cluster rejected the fault; a probe failure means the cluster
accepted it and nothing happened — a different bug with a different fix.

The fault is then **backed out**: an unverified fault is not a valid
experiment, and leaving it running would contaminate the next one while the
caller, holding an error and no UID, has no way to clear it. If the rollback
itself fails the lease is deliberately left in place so the reaper collects it
at the deadline, and the audit record says so with `left_to_reaper: true`.

A failing **SOT** probe is the cheaper case, and reads differently:

```
executor[precheck:precheck-failed]: probe "simian-reachable-before" (http)
never passed in 33.05s (7 polls)
```

Stage `precheck`, reason `precheck-failed`, and nothing to roll back — the
driver was never called, no object was created, no lease was taken. The fault
is simply refused.

A manifest carrying probes submitted to a controller with no prober wired in is
rejected with `probe-not-configured` rather than applied unverified. Skipping a
gate that cannot run would mark unverified faults verified, which is the whole
failure being prevented.

## The audit trail

One event per probe, pass or fail, carrying the observed value:
`fault.precheck` for SOT, `fault.efficacy` for Settle.

```json
{"event":"fault.efficacy","fault_uid":"f-01M1E9...","payload":{
  "probe":"payments crash-loops","type":"k8s","mode":"Settle","passed":true,
  "observed":"CrashLoopBackOff","expected":"\"CrashLoopBackOff\" in output",
  "attempts":7,"elapsed_ms":13840}}
```

The observed value, not just the boolean, is the point — a pass/fail flag
cannot be debugged once the arena is gone. For a dataplane gate it is the
whole record of the experiment:

```
fault.precheck  simian-reachable-before  passed  "payments-0 (http://10.244.1.7:8080/): 200 in 2ms"
fault.efficacy  simian-partitioned       passed  "payments-0 (http://10.244.1.7:8080/): unreachable after 3s: context deadline exceeded"
```

**An eval result whose fault carries no passing `fault.efficacy` record is not
a data point.** It is a harness bug, and downstream consumers should report it
as such rather than average it in.

## Timing

The lease deadline is *not* extended to cover the settle wait. A fault that
takes 30s to manifest spends 30s of its own duration doing so.

This keeps Simian's lease and the engine's own server-side `spec.duration` in
agreement — Chaos Mesh starts its clock at apply time regardless of what Simian
is waiting for, and letting the two drift apart would leave the fault
outliving its lease. The cost is visible as `elapsed_ms` on the efficacy event
rather than hidden as a discrepancy.
