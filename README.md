# Simian Agent

Chaos engineering for Kubernetes that knows what it did. Simian injects faults
into namespaces you opt in — *arenas* — checks that each fault actually took
effect, takes it back out on time, checks that the workload recovered, and
records all of it in an audit trail. It can plan faults itself with an LLM, or
apply the ones you ask for. That record is what makes it useful for the thing
it was built for: measuring the SRE agents that are supposed to diagnose what
went wrong.

**Latest release:** [v0.2.0](https://github.com/go-steer/simian-agent/releases/latest) ·
image `ghcr.io/go-steer/simian-agent:0.2.0` · Apache 2.0

## Try it

**[Getting started](https://go-steer.github.io/simian-agent/docs/getting-started/)**
([source](docs/site/content/docs/getting-started.md)) takes you from an empty
project to watching Simian break Online Boutique, check the damage and plan
its own faults — on **GKE** in about 30 minutes, or on **kind** on your laptop
in about 15. It is written as a runbook an agent can follow too; agents should
read [`AGENTS.md`](AGENTS.md) first.

## What it does

- **Four fault engines.** [Chaos Mesh](https://chaos-mesh.org/) (pod kills,
  network delay and partitions, CPU and memory stress, IO, time, DNS and HTTP
  faults); plain NetworkPolicy partitions; HTTP delay and abort through an
  Envoy sidecar; and `kube-state`, which synthesizes objects born broken — an
  image that does not exist, a rollout that never finishes, a claim that never
  binds — thirteen states no packet-level fault can produce.
  [Using the chaos engines](docs/site/content/docs/chaos-engines.md).
- **Safe by construction.** Faults only reach arenas, never excluded
  workloads, never above the permitted blast-radius tier or the duration
  ceiling, and one at a time in the recommended configuration. Simian refuses faults it can see it could
  not take back out — HTTPChaos on a liveness probe or a gRPC port, IOChaos on
  a read-only filesystem — and narrows a selector that would otherwise reach
  the whole namespace. [Design](docs/site/content/docs/design.md).
- **Verified, both ways.** Chaos Mesh's own injection status, probes that
  observe the effect (a connection timing out under a partition), and a
  recovery check after every fault ends.
  [Efficacy probes](docs/site/content/docs/efficacy-probes.md).
- **Autonomous mode.** Every cycle: health gate against a baseline, topology
  snapshot, an LLM-written hypothesis and plan, bounded execution — with the
  refusals and recent faults fed back so it does not repeat itself.
- **A record that survives.** The audit trail lives on a volume and reads back
  with `simian audit export`; a restarted controller adopts the faults its
  predecessor left running, from that record or from the cluster.
- **Built to grade agents.** Scenario packs with ground truth, and
  `simian-eval` to run them against a subject — k8s-lookout, k8s-sre-agent, or
  any command — and score its reports.
  [Eval substrate](docs/site/content/docs/eval-substrate.md).

## Documentation

| | |
|---|---|
| [Getting started](docs/site/content/docs/getting-started.md) | GKE and kind, end to end |
| [Deploying with Helm](docs/site/content/docs/deploy.md) | your own workloads as arenas; read access for triaging agents |
| [Using the chaos engines](docs/site/content/docs/chaos-engines.md) | every fault kind, directed and autonomous |
| [CLI reference](docs/site/content/docs/cli-reference.md) · [Helm values](docs/site/content/docs/helm-values.md) | every flag and value |
| [Known limitations](docs/site/content/docs/known-limitations.md) | read before relying on a fault kind |
| [Architecture](docs/site/content/docs/architecture.md) · [Design](docs/site/content/docs/design.md) · [Roadmap](docs/site/content/docs/roadmap.md) | how it works and where it is going |

The rendered site is at <https://go-steer.github.io/simian-agent/>.

## Building

```bash
make all                                                # build bin/simian and run the unit tests
golangci-lint run -c dev/tools/.golangci.yml ./...      # lint
make cluster && make e2e                                # kind end-to-end suite
dev/tools/verify-chart                                  # Helm chart assertions
```

See [Contributing](docs/site/content/docs/contributing.md) and [`AGENTS.md`](AGENTS.md).
