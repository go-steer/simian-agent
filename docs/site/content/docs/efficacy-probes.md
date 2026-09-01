---
title: "Efficacy probes"
linkTitle: "Efficacy probes"
weight: 75
description: "Settle probes gate Apply so a fault that silently did nothing is never reported as applied."
---

A fault that the cluster accepts and then quietly does nothing is the worst
outcome Simian can produce. It is not a missing measurement — it is a wrong
one. The eval result reads *"the agent missed a network partition"* when there
was no partition, and it averages in with the real data points.

Settle probes close that gap. A fault carrying them is not reported as applied
until Simian has *observed* it land.

## Efficacy, not outcome

Simian checks that the fault **landed**. It does not check what the fault
**caused**.

* **Efficacy** — is the pod genuinely in `CrashLoopBackOff`? Does the
  NetworkPolicy exist and select the right pods? Simian's business.
* **Outcome** — did latency rise, did the SLO burn, did the agent notice? The
  agent under test's business, and what it is scored on.

Measuring outcome here would let the harness grade its own experiment, so the
probe types are deliberately limited to things that answer "did it land".

## Writing a Settle probe

Probes hang off `FaultManifest.probes`. Mode `Settle` is the only mode Simian
schedules; the other Litmus modes round-trip untouched.

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

A manifest carrying Settle probes submitted to a controller with no prober
wired in is rejected with `probe-not-configured` rather than applied
unverified. Skipping a gate that cannot run would mark unverified faults
verified, which is the whole failure being prevented.

## The audit trail

One `fault.efficacy` event per probe, pass or fail, carrying the observed
value:

```json
{"event":"fault.efficacy","fault_uid":"f-01M1E9...","payload":{
  "probe":"payments crash-loops","type":"k8s","passed":true,
  "observed":"CrashLoopBackOff","expected":"\"CrashLoopBackOff\" in output",
  "attempts":7,"elapsed_ms":13840}}
```

The observed value, not just the boolean, is the point — a pass/fail flag
cannot be debugged once the arena is gone.

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
