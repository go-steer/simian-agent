---
title: simian-agent
---

{{< blocks/cover title="simian-agent" image_anchor="top" height="med" >}}

<p class="lead mt-5">
An autonomous chaos-engineering agent for Kubernetes. Provision an arena, deploy a System Under Test, and either describe a fault in plain English or let an LLM run the planning loop against your live cluster topology.
</p>

<a class="btn btn-lg btn-primary me-3 mb-4" href="docs/getting-started/">Get started <i class="fa-solid fa-arrow-right ms-2"></i></a>
<a class="btn btn-lg btn-secondary me-3 mb-4" href="https://github.com/go-steer/simian-agent">Source on GitHub <i class="fa-brands fa-github ms-2"></i></a>

{{< /blocks/cover >}}

{{% blocks/lead color="primary" %}}

`simian-agent` ships **directed-mode chaos** (plain-text intent → LLM-translated FaultManifest), an **autonomous planning loop** (health gate → topology snapshot → LLM-generated AttackPlan → bounded execution), three chaos engines (`chaos-mesh`, `network-policy`, `envoy-fault`), and a **safety stage** at the executor chokepoint that enforces eligibility, blast-radius tier, duration, and concurrency caps on every fault before it lands.

{{% /blocks/lead %}}

{{% blocks/section color="dark" type="row" %}}

{{% blocks/feature icon="fa-solid fa-shield-halved" title="Safe by default" url="docs/design/" %}}
Every fault flows through one chokepoint — the Fault Executor — which checks namespace eligibility, blast-radius tier, duration ceiling, and concurrency budget before any chaos driver runs. Cap violations surface as `executor.rejected` with a structured reason, not silent acceptance.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-robot" title="Autonomous or directed" url="docs/getting-started/" %}}
`simian chaos --intent "..."` for plain-text directed faults. `simian serve --autonomous` for the LLM-driven planning loop with per-cycle budget caps, baseline health-gating, and clean LLM-down skip behavior.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-network-wired" title="Works on GKE Dataplane V2" url="docs/dpv2-chaos-engines/" %}}
The `network-policy` engine handles partitions and the `envoy-fault` engine handles HTTP-layer delay + abort on clusters where Chaos Mesh's NetworkChaos is silently bypassed by the eBPF dataplane.
{{% /blocks/feature %}}

{{% /blocks/section %}}

{{% blocks/section %}}

## Install

```bash
# Build the binary
make all

# One-shot: create an arena, deploy Online Boutique, capture baseline
bin/simian sut deploy --namespace boutique-1 --create-arena
```

See [Getting started](docs/getting-started/) for the first chaos fault, [Deploying with Helm](docs/deploy/) for the in-cluster install, or jump to the [Design doc](docs/design/) for the architecture.

{{% /blocks/section %}}
