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
`simian-envoy-runtime`.

Defaults are on unless the operator turns them off:

```
simian serve --default-efficacy-probes=false
```

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
