---
title: Documentation
linkTitle: Documentation
weight: 1
menu:
  main:
    weight: 10
---

You're in the `simian-agent` reference docs. The site root has the marketing pitch; this section is the reference.

## Start here

**Brand new?** → [Getting started]({{< relref "getting-started.md" >}}) walks from `make all` through your first directed-chaos fault and your first autonomous-mode plan against an Online Boutique deployment.

**Installing in-cluster?** → [Deploying with Helm]({{< relref "deploy.md" >}}) covers the chart install patterns and the recommended overlay.

**Picking which chaos engine to use?** → [Using the chaos engines]({{< relref "chaos-engines.md" >}}) covers the directed and autonomous patterns for `chaos-mesh`, `network-policy`, and `envoy-fault` — including which one to reach for on which kind of cluster.

**Hit something weird?** → [Known limitations]({{< relref "known-limitations.md" >}}) collects the GKE Dataplane V2 NetworkChaos bypass, the Envoy injection vs gRPC probe interaction, and other gotchas worth knowing about before you debug.

## Reference index

### Getting things done
- **[Getting started]({{< relref "getting-started.md" >}})** — first chaos in under 10 minutes.
- **[Deploying with Helm]({{< relref "deploy.md" >}})** — in-cluster install patterns.
- **[Using the chaos engines]({{< relref "chaos-engines.md" >}})** — directed and autonomous patterns per engine.
- **[CLI reference]({{< relref "cli-reference.md" >}})** — every flag on every subcommand.
- **[Helm values reference]({{< relref "helm-values.md" >}})** — every chart value.

### Concepts and design
- **[Design]({{< relref "design.md" >}})** — architecture: Fault Executor chokepoint, LLM Provider contract, chaos drivers, MCP surface, deployment topology.
- **[Requirements]({{< relref "requirements.md" >}})** — scope, operating modes, deployment postures, the `R-*` requirements catalog.
- **[Roadmap]({{< relref "roadmap.md" >}})** — phased development plan; what's shipped, what's next.
- **[DPv2-compatible chaos engines]({{< relref "dpv2-chaos-engines.md" >}})** — why we built `network-policy` and `envoy-fault`, and what they replace.

### Operating
- **[Known limitations]({{< relref "known-limitations.md" >}})** — cluster-side gotchas, dataplane caveats, feature limitations.

### Working on Simian
- **[Contributing]({{< relref "contributing.md" >}})** — how to file issues, structure PRs, and keep the recommended-defaults overlay honest.
