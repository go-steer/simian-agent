---
title: "Seeing what Simian is doing"
linkTitle: "Seeing what it does"
weight: 25
description: "Every way to see what Simian has injected, what autonomous mode decided and why, and whether anything was left broken — live, after the fact, and on a dashboard."
---

Simian records everything it does as audit events. Every view on this page
reads those same events, so they cannot disagree with each other: a dashboard,
the audit export and `simian watch` all tell the same story.

| You want to… | Use |
|---|---|
| watch an arena live | [`simian watch`](#live-simian-watch) |
| know what autonomous mode decided, and why a cycle was skipped | [`simian audit export --cycles`](#what-autonomous-mode-decided) or `simian watch` |
| know exactly what was injected, when, and how it ended | [`simian audit export`](#what-was-injected) |
| graph it, or be paged when a fault leaves something broken | [Prometheus metrics](#metrics-and-alerts) |
| ask from an agent or another tool | the [MCP tools](#from-an-agent-mcp) |
| read the raw events | [the controller log](#the-raw-events) |

The commands assume the CLI can reach the controller — see step 4 of
[Getting started]({{< relref "getting-started.md" >}}) (`SIMIAN_MCP_URL` and a
port-forward).

## Live: `simian watch`

```bash
bin/simian watch --namespace boutique
```

Redraws every few seconds (`Ctrl-C` to leave):

```
simian watch — boutique   (updated 18:29:49)
────────────────────────────────────────────────────────────────────────
ACTIVE FAULTS (1)
  f-01M497MC2X  chaos-mesh       PodChaos         → adservice             [1m1s remaining]

AUTONOMOUS CYCLES (4)
  latest plan (18:28:10):
    If the adservice is unavailable, the frontend will handle the
    error gracefully and continue to serve user requests for other
    product features without significant latency or errors.
    1. PodChaos → adservice for 1m30s
       why that long: The pod-kill action is instantaneous. The 90-second duratio…
  18:28:10  completed PodChaos→adservice (1 applied)
  18:26:10  completed NetworkChaos→paymentservice (1 applied)
  18:24:10  skipped   budget-full
  18:22:10  completed NetworkChaos→currencyservice (1 applied)

RECENT FAULTS (4)
  18:29:20  f-01M497MC2X chaos-mesh       PodChaos         → adservice             applied
  18:28:40  f-01M497FE8N chaos-mesh       NetworkChaos     → paymentservice        cleared (deadline-reached)
  18:26:10  f-01M497AJ44 chaos-mesh       NetworkChaos     → currencyservice       cleared (deadline-reached)
  18:23:10  f-01M49770TJ chaos-mesh       PodChaos         → productcatalogservice  cleared (deadline-reached)
```

The cycles section is what autonomous mode is thinking: the latest hypothesis
and plan in full, then one line per recent cycle with what it applied or why
it was skipped. It needs a v0.1.14 or newer controller.

## What autonomous mode decided

```bash
kubectl -n simian-system exec deploy/simian-controller -- \
    simian audit export --cycles /var/lib/simian/audit.jsonl
```

One row per cycle and arena: when it started, its outcome (`completed`,
`skipped`, `unfinished` if the controller restarted mid-cycle), the skip
reason, the plan's steps, how many steps were applied and refused, and the
hypothesis — or, for a skipped cycle, what the health gate, the LLM or the
plan validator said. `--format json` adds each step's rationale and the reason
for its duration.

| Skip reason | Meaning |
|---|---|
| `health-gate` | something in the arena was not Ready against its baseline — often the previous fault's pod still restarting |
| `budget-full` | the concurrency limit was reached (one fault at a time in the recommended configuration) |
| `no-valid-plan` | the LLM answered, but none of its plans passed validation; the detail says what was wrong |
| `llm-unavailable` | the LLM call failed (quota, permissions, network) |
| `interrupted` | the controller was shutting down |

## What was injected

```bash
kubectl -n simian-system exec deploy/simian-controller -- \
    simian audit export /var/lib/simian/audit.jsonl
```

One row per fault: source (autonomous or directed), kind, target, how it ended
(`expired deadline-reached` is the normal end; `refused` with the safety
check's reason; `cleared` early), whether Chaos Mesh confirmed the injection,
whether Simian's probes saw the effect, and **whether the workload was Ready
again afterwards** (`RECOVERED`). `--since 24h` limits it; `--format json` adds
the full spec and, for a failed recovery, which pods stayed down and why.

The audit file lives on the controller's volume and survives restarts. Both
exports also read the controller's log: `kubectl -n simian-system logs
deploy/simian-controller | bin/simian audit export --cycles`.

## Metrics and alerts

The controller serves Prometheus metrics on port `9090` at `/metrics`
(`metrics.enabled` in the chart, on by default). The pod carries the usual
`prometheus.io/scrape` annotations; on GKE, set
`--set metrics.podMonitoring.enabled=true` and Managed Service for Prometheus
collects them, queryable in Cloud Monitoring. With prometheus-operator, use
`metrics.serviceMonitor.enabled=true`.

| Metric | Labels | What it counts |
|---|---|---|
| `simian_faults_applied_total` | `namespace`, `engine`, `kind`, `source` | faults that reached the cluster |
| `simian_faults_ended_total` | `namespace`, `kind`, `outcome`, `reason` | faults that ended: `expired`, `cleared`, `refused`, `driver-failed` |
| `simian_fault_recovery_checks_total` | `namespace`, `kind`, `passed` | recovery checks after a fault; `passed="false"` is a fault that left something broken |
| `simian_cycles_total` | `namespace`, `outcome`, `reason` | autonomous cycles, completed or skipped and why |
| `simian_active_faults` | `namespace` | faults held right now |
| `simian_build_info` | `version` | the running version |

Useful queries:

```promql
# faults applied per hour, by kind
sum by (kind) (increase(simian_faults_applied_total[1h]))

# share of cycles skipped, by reason, over the last hour
sum by (reason) (increase(simian_cycles_total{outcome="skipped"}[1h]))
  / ignoring(reason) group_left sum(increase(simian_cycles_total[1h]))

# refusals, by reason — the safety checks at work
sum by (reason) (increase(simian_faults_ended_total{outcome="refused"}[1d]))
```

Alerts worth having:

```yaml
- alert: SimianFaultLeftWorkloadBroken      # a fault ended and its pods did not come back
  expr: increase(simian_fault_recovery_checks_total{passed="false"}[15m]) > 0
- alert: SimianAutonomousModeStalled         # on, but nothing applied for an hour
  expr: sum(increase(simian_cycles_total[1h])) > 0 and sum(increase(simian_faults_applied_total[1h])) == 0
```

A broken workload after a fault is the one that needs a person: the JSON audit
export names the pods, and deleting them usually restores them.

## From an agent: MCP

The controller's MCP endpoint (port `8081`) answers the same questions as
tools any MCP client can call: `list_active_faults`, `get_recent_faults`,
`get_recent_cycles` (the cycle view above), `get_topology`, `get_baseline`.
`simian watch` is built on them.

## The raw events

```bash
kubectl -n simian-system logs deploy/simian-controller | grep '"msg":"audit"'
```

Every event is one JSON line: `cycle.started`, `plan.generated`,
`executor.received`, `executor.validated`, `driver.applied`, `fault.injected`,
`fault.efficacy`, `lease.expired`, `fault.recovered`, `cycle.skipped`, and the
rest. On GKE the controller's stdout reaches Cloud Logging; in Logs Explorer:

```
resource.labels.namespace_name="simian-system"
jsonPayload.event="plan.generated"
```

## Chaos Mesh's dashboard

Chaos Mesh installs its own dashboard (`chaos-dashboard`), which shows its
experiments as they run:

```bash
kubectl -n chaos-mesh port-forward svc/chaos-dashboard 2333:2333
```

It sees only Chaos Mesh's side — not Simian's network-policy or kube-state
faults, and nothing of why a fault was chosen — and it shows the experiments'
objects, which is exactly what a triaging agent must not read (see
[Agents that triage the cluster]({{< relref "deploy.md#agents-that-triage-the-cluster" >}})).
Keep it for operators.

## Not yet

There is no web UI ([design]({{< relref "web-ui-design.md" >}})), and Simian
does not write Kubernetes Events: events in an arena would be readable by any
agent with the `view` role, and would hand it the answer.
