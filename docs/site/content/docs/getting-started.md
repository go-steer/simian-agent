---
title: "Getting started"
linkTitle: "Getting started"
weight: 10
description: "From clone to your first chaos fault in under 10 minutes."
---

This walks from a fresh clone through your first directed-chaos fault, then through your first autonomous-mode plan. Assumes you have a Kubernetes cluster with [Chaos Mesh](https://chaos-mesh.org/) installed and your kubeconfig points at it (cluster-admin or equivalent).

## Build

```bash
make all
```

Produces `bin/simian`. The binary holds every subcommand (`arena`, `sut`, `serve`, `chaos`, `plan`).

## Provision an arena and deploy a SUT

An *arena* is a namespace annotated `simian.chaos/eligible="true"` plus the RBAC needed for the controller's chaos service account. A *SUT* is a System Under Test deployed into that arena (Online Boutique is the built-in default).

```bash
# One-shot: create the arena, deploy Online Boutique, capture baseline.
bin/simian sut deploy --namespace boutique-1 --create-arena
```

For more granular control, `simian arena create/destroy/describe` and `simian sut list/deploy/destroy` can be invoked independently.

## Start the controller

```bash
# Source your LLM credentials (Vertex/ADC or GEMINI_API_KEY).
source ~/scripts/gemini.sh

# Start the controller. With no --eligible-namespace flag, it honors the
# annotation set by `arena create` (live, no restart needed).
bin/simian serve
```

The controller serves the [MCP](https://modelcontextprotocol.io/) interface on `:8081` plus the autonomous loop (when `--autonomous` is set).

## Apply your first fault — directed mode

Two paths: LLM-translated (plain-text intent → fault) or deterministic-control (hand-built manifest).

```bash
# LLM-translated: plain English intent
bin/simian chaos --intent "kill one paymentservice pod in boutique-1 for 30 seconds" \
                 --namespace boutique-1

# Deterministic-control: submit a fully-formed manifest
bin/simian chaos --manifest examples/network-latency-manifest.json
```

`simian chaos` returns a fault UID; the lease auto-expires after the manifest's duration (default 2m, capped by `--duration-ceiling`). Inspect with `simian chaos --list-active`; clear early with `simian chaos --clear <uid>`.

## Apply your first fault — autonomous mode

```bash
# Set up arena + SUT, capture baseline IN the controller process so the
# autonomous loop can read it via get_baseline.
bin/simian sut deploy --namespace boutique-1 --create-arena --use-controller

# Dry-run plan: emit an AttackPlan as JSON, do NOT apply.
bin/simian plan --namespace boutique-1 \
                --hypothesis "frontend tolerates one cartservice pod restart"

# Run the autonomous loop (every 90s; serializes at MaxConcurrentFaults=1).
bin/simian serve --autonomous --autonomous-namespace boutique-1 \
                 --cycle-interval 90s \
                 --max-faults-per-cycle 2 \
                 --max-severity-per-cycle namespace
```

Each autonomous cycle: health gate → topology snapshot → LLM plan generation → bounded execution under the executor's safety stage. Plans are always emitted (and audit-logged) before any fault is applied.

## Tear down

```bash
# Refuses if simian-managed faults are still leased — clear them first
# with 'simian chaos --clear <uid>' or pass --force to override.
bin/simian sut destroy --namespace boutique-1 --with-arena
```

## Next

- [Deploying with Helm]({{< relref "deploy.md" >}}) — the in-cluster controller install path.
- [Using the chaos engines]({{< relref "chaos-engines.md" >}}) — directed and autonomous patterns for each of `chaos-mesh`, `network-policy`, and `envoy-fault`.
- [CLI reference]({{< relref "cli-reference.md" >}}) — every flag on every subcommand.
- [Design]({{< relref "design.md" >}}) — architecture, the Fault Executor chokepoint, the LLM Provider contract.
