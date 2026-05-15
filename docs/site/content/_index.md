---
title: simian-agent
toc: false
---

# simian-agent

An autonomous chaos-engineering agent for Kubernetes, built on top of [Chaos Mesh](https://chaos-mesh.org/).

`simian-agent` provisions an isolated arena (a namespace with the right RBAC and admission backstops), deploys a System Under Test, and then either applies a directed fault you describe in plain text or runs an autonomous loop that uses an LLM to generate plans against the live cluster topology.

[View on GitHub →](https://github.com/go-steer/simian-agent)

---

## Status

Documentation is being assembled. Until the site is populated, the canonical references are:

- The [README](https://github.com/go-steer/simian-agent#readme) — quick-start, project layout, and verified setups.
- The [roadmap](https://github.com/go-steer/simian-agent/blob/main/docs/roadmap.md) — milestone log of what's shipped and what's next.
- The [design notes](https://github.com/go-steer/simian-agent/tree/main/docs) — `design.md`, `requirements.md`, `simian-agent.md`.
