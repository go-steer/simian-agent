---
title: "CLI reference"
linkTitle: "CLI reference"
weight: 80
description: "Every flag on every simian subcommand."
---

`simian` is a single binary with cobra subcommands. This page is generated from `simian <cmd> --help` output. There is a second binary, `simian-eval`, covered at the end: it drives a scenario pack against a subject and is deliberately not part of the operator binary.

To get the most up-to-date reference for any single command, run it with `--help`:

```bash
simian --help
simian serve --help
simian chaos --help
simian sut deploy --help
```

## Subcommand index

| Subcommand | Purpose |
|---|---|
| `simian arena` | Manage chaos arena namespaces (create/destroy/describe). The arena is the namespace+RBAC unit of isolation for chaos. |
| `simian sut` | Manage Systems Under Test (deploy/destroy/list). Built-in SUT: Online Boutique. |
| `simian serve` | Run the controller: Fault Executor + MCP server + autonomous loop. |
| `simian chaos` | Submit a fault either as plain-text intent (LLM-translated) or as a hand-built FaultManifest (deterministic-control). Also list/clear active faults. |
| `simian plan` | Generate an `AttackPlan` against a real arena and emit it as JSON. Default `--dry-run=true` does not apply. |
| `simian evaluate` | Score a finished run offline from its audit log and the subject's report. Contacts no cluster. |

## Common flag patterns

### Eligibility

`--eligible-namespace <ns>` (repeatable, `simian serve`) overrides the default annotation-based lookup. Without it, the controller treats any namespace with `simian.chaos/eligible="true"` as eligible.

### LLM provider

`--llm-provider gemini|stub` (default `gemini`); `--llm-model <id>` overrides the default `gemini-2.5-pro`. Vertex/ADC and API-key auth are both supported (Vertex preferred for production via Workload Identity).

### Executor safety policy

Set on `simian serve`:

| Flag | Default | Notes |
|---|---|---|
| `--duration-ceiling` | 15m | Hard cap per fault. |
| `--max-concurrent-faults` | 0 (no cap) | Total leased faults across namespaces. Rejected applies surface as `executor.rejected` with reason `safety:budget-exceeded`. |
| `--min-cooldown` | 0 | Per-namespace cooldown between consecutive faults. An apply already in flight against a namespace counts as consecutive. |
| `--permitted-tiers` | namespace,node | Blast-radius tiers this installation permits (`namespace\|node\|external`). Repeatable or comma-separated. Unset keeps the default; pass just `namespace` to keep node-level chaos off the cluster entirely. An unrecognised name stops the controller starting rather than falling back — see [Helm values]({{< relref "helm-values.md" >}}). |
| `--default-efficacy-probes` | true | Attach Simian's own [efficacy gate]({{< relref "efficacy-probes.md" >}}) to every fault kind that has one. Set false to accept faults that report success without proving they landed. |

### Autonomous mode

Set on `simian serve` together:

| Flag | Default | Notes |
|---|---|---|
| `--autonomous` | false | Enable the planning loop. |
| `--autonomous-namespace` | (required when `--autonomous`) | Repeatable. Arena namespace(s) the loop targets. |
| `--cycle-interval` | 5m | Time between cycles. |
| `--max-faults-per-cycle` | 3 | Cap on faults applied per cycle. |
| `--max-severity-per-cycle` | namespace | Highest blast tier the loop will apply (`namespace\|node\|external`). Validated at startup: an unparseable cap would make the loop skip every step, which is indistinguishable from a planner producing nothing. |
| `--hypothesis-hint` | empty | Soft preference passed to the LLM each cycle. Useful for biasing toward specific engines. |

### Envoy SUT injection

| Flag | Where | Default | Notes |
|---|---|---|---|
| `--no-envoy-faults` | `simian sut deploy` | true (skip) | Inverted flag. Set `--no-envoy-faults=false` to opt INTO injection. Default off because of the gRPC-probe limitation. |
| `--sut-inject-envoy-faults` | `simian serve` | false | Controller-side policy. Set to true to inject Envoy when SUTs are applied via the `establish_baseline` MCP tool. |

See [Known limitations]({{< relref "known-limitations.md" >}}) for why these default off.

### Submitting a fault

`simian chaos` accepts three input shapes:

```bash
# 1. LLM-translated path
simian chaos --intent "kill one paymentservice pod for 30 seconds" --namespace boutique-1

# 2. Deterministic-control path with engine + kind + spec
simian chaos --engine chaos-mesh --kind PodChaos \
             --api-version chaos-mesh.org/v1alpha1 \
             --namespace boutique-1 --workload paymentservice \
             --duration 30s \
             --spec '{"action":"pod-kill","mode":"one","selector":{"labelSelectors":{"app":"paymentservice"}}}'

# 3. Submit a fully-formed manifest
simian chaos --manifest examples/network-latency-manifest.json
```

Plus the inspection / management subcommands:

```bash
simian chaos --list-active     # all leased faults
simian chaos --list-catalog    # catalog the LLM sees (all engines)
simian chaos --clear f-<UID>   # clear before lease expiry
```

`--spec`, `--spec-file`, and `--stdin-spec` are mutually exclusive — set at most one. The CLI rejects overlapping inputs upfront rather than silently picking one.

### Scoring a run

```bash
simian evaluate --pack packs/parity --audit run.log --report agent.json
simian evaluate --pack packs/parity --audit - --report agent.json --format json
```

Two artifacts, joined on the scenario ID the audit sink stamps onto every
event: the audit log says which faults landed and when, the report says what
the subject found and when. Nothing is executed and no kubeconfig is read, so
the same artifacts produce the same scorecard on any machine, any time later.

A scenario whose fault has no passing efficacy record prints as `NOT SCORED`
rather than as a miss — the cluster was never broken, so a zero would mean
"nothing to find" while reading as "the agent missed it". Below
`--min-efficacy` (default `0.8`) the scorecard is still printed, then the
command exits non-zero: the numbers measure the harness, not the subject. Pass
`--min-efficacy 0` to report anyway; the warning stays either way.

### Tearing down

```bash
simian sut destroy --namespace boutique-1                # SUT only
simian sut destroy --namespace boutique-1 --with-arena   # both layers
```

`destroy --with-arena` refuses if simian-managed faults are still leased; pass `--force` to override (after clearing them with `simian chaos --clear`).

## `simian-eval` — running a pack against a subject

A second binary. Cluster lifecycle, subject processes and scoring have no
business linking into the operator binary that runs in-cluster with chaos RBAC.

```bash
simian-eval --pack packs/parity --subject exec:./bin/lookout --out runs/
simian-eval --pack packs/parity,packs/lookout --subject exec:./bin/agent --concurrency 4
simian-eval --pack packs/parity --subject noop: --cluster kind --out runs/floor
simian-eval --pack packs/parity --subject exec:./bin/agent --only parity-0003
```

Per scenario: provision an arena namespace, inject the faults **through the
normal executor path** — same validation, same safety stages, same leases, same
efficacy gates — hand the subject the prompt, collect its report, then clear the
chaos and put the namespace back. Two files land in `--out`: `audit.log` is
Simian's side, `run.json` is the subject's, and they join on the scenario ID.
The scorecard printed at the end comes from reading those two files back, so
`simian evaluate` reproduces it exactly, with or without the cluster.

| Flag | Default | Notes |
|---|---|---|
| `--pack` | (required) | Pack directory. Repeatable or comma-separated; several packs run as one suite. Two packs sharing a scenario ID is a load error, not a merge. |
| `--subject` | (required) | `exec:<command line>`, or `noop:` for the zero-score floor. |
| `--only` | (all) | Scenario IDs to run. An ID that is not in the pack is an error, not an empty run. |
| `--out` | `runs/<timestamp>` | Where `audit.log` and `run.json` go. |
| `--cluster` | `current` | `current` uses the kubeconfig's cluster and leaves it standing; `kind` provisions a throwaway cluster for the run and deletes it afterwards, including on Ctrl-C. |
| `--concurrency` | 1 | Ceiling, not a target: scenarios sharing a namespace are serialised regardless, and a control takes the cluster to itself. |
| `--subject-timeout` | 10m | How long one investigation may take before the subject is killed and scored as a failure. |
| `--subject-dir`, `--subject-env` | — | Working directory and extra `KEY=VALUE` environment for an `exec:` subject. |
| `--remediation-poll` | 5s | How often to ask whether the fault is gone while the subject works, for time-to-remediate. `0` disables the watch. |
| `--eligible-namespace` | (annotation) | Fence the run to a fixed namespace list instead of reading `simian.chaos/eligible`. Reach for this when the cluster has other tenants. |
| `--keep-arenas` | false | Leave arena namespaces standing afterwards, for poking at a scenario that went wrong. Faults are still cleared. |
| `--allow-short-faults` | false | Permit faults shorter than `--subject-timeout`. Off by default: the lease expires mid-investigation, the reaper clears it, and that disappearance is recorded as the subject having remediated it. |
| `--score` | true | Off writes the artifacts and stops. |
| `--format`, `--min-efficacy` | `text`, `0.8` | As `simian evaluate`. |

Namespaces the run creates are annotated with `simian.chaos/eval-run=<run id>`
and destroyed at the end. A namespace that already existed is annotated and
**left standing** — a rig that deletes namespaces it merely found is one bad
scenario file away from deleting something that mattered.
