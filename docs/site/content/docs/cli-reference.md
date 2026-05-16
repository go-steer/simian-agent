---
title: "CLI reference"
linkTitle: "CLI reference"
weight: 80
description: "Every flag on every simian subcommand."
---

`simian` is a single binary with cobra subcommands. This page is generated from `simian <cmd> --help` output.

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
| `simian evaluate` | Stub until M5 (scenario data export). |

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
| `--min-cooldown` | 0 | Per-namespace cooldown between consecutive faults. |

### Autonomous mode

Set on `simian serve` together:

| Flag | Default | Notes |
|---|---|---|
| `--autonomous` | false | Enable the planning loop. |
| `--autonomous-namespace` | (required when `--autonomous`) | Repeatable. Arena namespace(s) the loop targets. |
| `--cycle-interval` | 5m | Time between cycles. |
| `--max-faults-per-cycle` | 3 | Cap on faults applied per cycle. |
| `--max-severity-per-cycle` | namespace | Highest blast tier the loop will apply (`namespace\|node\|external`). |
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

### Tearing down

```bash
simian sut destroy --namespace boutique-1                # SUT only
simian sut destroy --namespace boutique-1 --with-arena   # both layers
```

`destroy --with-arena` refuses if simian-managed faults are still leased; pass `--force` to override (after clearing them with `simian chaos --clear`).
