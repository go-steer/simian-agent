---
title: "Architecture at a glance"
linkTitle: "Architecture at a glance"
weight: 12
description: "Three entry points, who launches whom, and which credentials each one holds."
---

Simian is one repository holding three programs that do different jobs against
the same drivers. Most confusion about it comes from assuming there is one.

| Entry point | Where it runs | Does it launch a subject? | What it is for |
| --- | --- | --- | --- |
| `simian serve` | a container, in-cluster | **no** | a chaos plane an agent talks to |
| `simian-eval` | your machine | **yes** — forks it | running a scenario pack against a subject and scoring it |
| `simian evaluate` | anywhere, no cluster | no | re-scoring a finished run from its artifacts |

Two words are used throughout and mean specific things:

- **adversary** — Simian. It breaks the cluster, and in eval mode it also grades.
- **subject** — whatever is being graded. A role, not a product. Today its
  occupants are `k8s-lookout` (the deterministic floor), `k8s-sre-agent`, and
  `noop`.

## `simian serve` — the chaos plane

```
   agent (any)
       │  MCP
       ▼
┌─────────────────────┐        ┌──── cluster ─────────────────┐
│ simian serve        │        │  arena namespace             │
│  Fault Executor     │──────▶ │    substrate pods            │
│  MCP server         │        │    chaos CRs                 │
│  ServiceAccount:    │        │    Role: simian-chaos        │
│   simian-provisioner│        └──────────────────────────────┘
└─────────────────────┘
```

Deployed by the Helm chart. It injects faults on request and knows nothing
about scenarios, subjects, expectations or scores. An agent connects over MCP,
asks for chaos, and is on its own.

This is the "generate chaos and let any agent deal with it" shape. It is the
older half of the project and the one with a real in-cluster identity: the
chart's `simian-provisioner` ServiceAccount can create namespaces and
Roles but is **read-only** on `chaos-mesh.org`. Injection is delegated to the
chaos controller's own ServiceAccount through a Role that Simian creates inside
each arena and deletes with it. That Role is the blast-radius jail.

## `simian-eval` — the graded loop

```
┌─ your machine ──────────────────────────────────────────────────┐
│                                                                  │
│   simian-eval  ──── fork + argv + env ────▶  subject process     │
│   (adversary)                                 k8s-sre-agent      │
│        │                                      k8s-lookout        │
│        │                                          │              │
│        │ --kubeconfig                             │ its own      │
│        │ (today: your admin file)                 │ kubeconfig   │
└────────┼──────────────────────────────────────────┼──────────────┘
         │                                          │
         ▼                                          ▼
   ┌──────────────────────── cluster ─────────────────────────┐
   │  arena namespace                                         │
   │    substrate pods  ◀─── chaos CR ───  (the answer,       │
   │                                        readable)         │
   └──────────────────────────────────────────────────────────┘
```

The subject is a **separate OS process that Simian forks**. Contact between
them is three things and nothing else: argv, environment, and a JSON file the
subject writes. Simian does not import the subject and the subject does not
import Simian — an adversary sharing a prompt template, a model client or a
Kubernetes client version with its subject can fail in a correlated way and
produce an eval that passes for the wrong reason.

### Why the adversary owns the window

Per scenario, `pkg/harness/runner.go`:

```
deploy substrate
inject fault          ──▶ blocks until the Settle probes pass
stamp InjectedAt           (a real request returned 503, not "the CR says Injected")
start watchRemediation ──▶ runs concurrently; sees what the subject changes
Subject.Investigate()  ──▶ the fork; blocks until the subject answers
stamp DetectedAt
clear chaos                (the answer ends the scenario, not the lease expiring)
```

Four of the seven measures need this. `time_to_detect`, `time_to_remediate`
and `efficacy_rate` are unobtainable after the fact, because only the injector
knows when the fault landed and whether it landed at all. And asking only once
the Settle probes pass is what separates "the subject missed it" from "the
fault had not arrived yet."

A chaos tool that does not own this window can inject faults. It cannot tell
you whether an agent got better.

## `simian evaluate` — scoring, decoupled

```
audit log (JSONL) ─┐
                   ├──▶ simian evaluate ──▶ scorecard
subject report ────┘        (pure; no cluster)
```

Scoring is a pure function of the artifacts, so a run can be re-scored months
later on another machine and produce the number the live rig produced. This is
how a new measure is validated against runs that predate it.

## Who holds which credentials

| | identity today | designed? |
| --- | --- | --- |
| `simian serve` | `simian-provisioner` SA + per-arena Role | yes |
| `simian-eval` | the operator's kubeconfig | no |
| the subject | whatever it brings; usually the same operator kubeconfig | no |

The eval harness cannot borrow the provisioner's identity: it injects with its
own client rather than delegating, so it needs `create` on `chaos-mesh.org` and
on `apps/deployments`, which the provisioner deliberately lacks. A scoped
identity for the eval path would be roughly the union of the provisioner Role
and the arena Role, and it does not exist yet.

The subject holding the operator's kubeconfig is a measurement problem rather
than a safety one: the chaos CR is an object in the namespace the subject is
asked about, and its spec is the answer. The `oracle_read` measure scores a
subject that names one.

## What is not built

- **`http:` and `mcp:` subjects** return "not implemented yet". Every subject
  today must be a local binary Simian can fork.
- **An in-cluster eval harness.** HTTP probes dial `Status.PodIP` directly, so
  the harness needs pod-network reachability — an argument for moving it into
  the cluster. But a harness in a pod cannot fork a binary from your disk, so
  it would have to reach the subject over the network. The two missing adapters
  above are that blocker.
