# AGENTS.md — simian-agent

Instructions for AI agents working with this repository: setting Simian up for
someone, or changing its code.

## What this project is

Simian is a chaos engineering controller for Kubernetes. It injects faults
into *arenas* — namespaces an operator opted in — through Chaos Mesh, plain
NetworkPolicies, an Envoy sidecar, or objects synthesized broken; checks each
fault took effect; takes it back out on time; checks the workload recovered;
and records all of it in an audit trail. It can plan faults itself with an
LLM (autonomous mode) or apply the ones it is given (directed mode). It is
built to be the ground truth that SRE agents are measured against.

## Setting Simian up for a user

Follow [`docs/site/content/docs/getting-started.md`](docs/site/content/docs/getting-started.md).
It is a runbook, validated end to end on GKE and on kind: every step has a
**Check** and an **If not**. Use it as written.

Rules:

- **Ask before creating anything billable or touching an existing cluster.**
  Confirm the GCP project, zone and cluster name with the user before
  `gcloud container clusters create`, and never install into a cluster they
  did not name for this. Keep `KUBECONFIG` pointed at the file the runbook
  sets, so no command can reach another of the user's clusters.
- **Never echo, log or commit credentials.** Ask the user for a Gemini API
  key or to authenticate `gcloud`; pass the key to the cluster as the runbook
  does (a Secret), not on a command line that ends up in shell history or a
  transcript.
- **Do not skip a failing Check.** Each step depends on the one before; stop,
  use the **If not** line, and tell the user if that does not resolve it.
- **Autonomous mode spends LLM calls and keeps injecting faults.** Turn it off
  (step 9's last command) before leaving the user's cluster, unless they asked
  for it to keep running.
- **Clean up what you created** (step 12), including
  `dev/tools/grant-vertex-access --revoke` on GKE: the IAM grant is
  project-wide and outlives the cluster. Report anything you could not delete.
- **Pin the release** the runbook names (`git clone --branch v…`). The chart,
  the CLI and the image must come from the same version.
- If the user's own agent will diagnose the faults, give it read access
  without the `chaos-mesh.org` API group — see "Agents that triage the
  cluster" in [`docs/site/content/docs/deploy.md`](docs/site/content/docs/deploy.md).

When it works, show the user the audit export (step 7) and what autonomous mode
decided (step 9): that is what they came to see.

## Changing the code

- `make all` builds and runs the unit tests; lint with
  `golangci-lint run -c dev/tools/.golangci.yml ./...`; `make cluster` and
  `make e2e` run the kind end-to-end suite. `dev/tools/verify-chart` checks the
  Helm chart.
- Every fault goes through `pkg/executor` (validate → safety → reserve →
  narrow → precheck → driver → confirm → settle → recovery). Do not add a path
  that applies a fault without it; safety checks belong there, not in drivers
  or the CLI.
- Faults must be reversible and bounded: a lease with a deadline, a driver
  `Clear`, and for engines that leave objects in the cluster, an orphan reaper
  and `ListLive` so a restarted controller can find them.
- The audit trail is the product's record of what it did. A change that alters
  what happens to a fault must alter what the audit trail says about it.
- Design and rationale: [`docs/site/content/docs/design.md`](docs/site/content/docs/design.md),
  [`known-limitations.md`](docs/site/content/docs/known-limitations.md).
