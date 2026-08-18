---
title: "Web UI design"
linkTitle: "Web UI (design)"
weight: 60
description: "Design doc for a single browser surface serving two purposes: live operator dashboard over `simian serve`, and the eval scorecard over run artifacts."
---

> **Status:** draft, 2026-07-17. Not implemented. Companion to [`design.md`]({{< relref "design.md" >}}) and the [Roadmap]({{< relref "roadmap.md" >}}).
>
> **Amended 2026-08-18:** scope decision — **one site, two purposes.** The eval
> scorecard from [`eval-substrate.md`]({{< relref "eval-substrate.md" >}}) §8.4 is
> a view in *this* application, not a second one. See §"One site, two data
> sources" below.

## Why a web UI

Simian's operator-facing surface today is CLI-only:

- `simian chaos --engine X --kind Y --spec '{...}'` for manual/directed submission.
- `tail -f serve.log | jq '.event'` for watching autonomous mode.
- `simian baseline show`, `simian sut list`, etc. for state inspection.

Two friction points recur, both surfaced by first-time operator use:

1. **Manual submission is unpleasant.** Writing engine-native spec JSON on the command line is fine once you've done it 20 times; hostile the first 20. There's no discovery ("what fault types are available for this workload?"), no form-filling with hints ("this field takes a duration; here's an example"), no confirmation preview.
2. **Autonomous mode is opaque.** When the loop is running, "what's the agent doing right now" requires `tail | jq` gymnastics. Active faults, current cycle status, next-cycle countdown, recent plans, topology-with-envoy-flag — all buried in structured logs.

A browser UI addresses both, and the transport is already in place: `simian serve` publishes MCP over SSE on `:8081/sse`. A web frontend is a *thin client* over that same protocol — no new server-side machinery required on the agent side.

## Non-goals

- **Not a replacement for the CLI.** Deterministic-control users, scripted runs, CI/CD integrations continue to speak MCP directly. The web UI is one client among several.
- **Not a full observability stack.** Prometheus / Grafana own metrics visualization; the web UI is a purpose-built operator surface for *this agent's* live state, not a general-purpose dashboard.
- **Not a plan editor.** Autonomous plans are LLM-authored; the web UI can render them (and eventually approve/reject them, once a plan-first gate lands), but not compose them by hand. Directed submissions remain the manual authoring path.

## Three customers, one UI

| Customer | Uses UI for | Views they need |
|---|---|---|
| **Manual operator** (game-day driver, incident responder) | Discover + submit a fault, watch its effect, clear it | Catalog picker → spec form with hints → confirm-and-apply → status watch → clear button |
| **Autonomous observer** (SRE on-call, resilience-team lead) | Watch what the running loop is doing, spot-check safety, take over if needed | Live audit stream, active faults with countdowns, current cycle progress, recent plans with rationale, topology + baseline diff, "pause" and "clear all" affordances |
| **Eval reviewer** (agent developer, benchmark reader) | Read a finished `simian-eval` run: what we broke, what each subject detected, diagnosed, and fixed | Scorecard grid (scenarios × subjects), per-scenario drill-down into the injected fault + ground truth + each subject's report, efficacy column, time-to-detect / time-to-remediate |

The first two share the same live data (audit stream, active-faults, topology, catalog, baseline) and differ mostly in which panels are prominent and whether a submit form is present. The third reads a finished run instead of a live server — see below. All three ship as tabs in one app, not as separate apps.

## One site, two data sources

The scorecard is not a second application. It is the same shell, the same
components, and the same event vocabulary, pointed at a **recording** instead of
a **live server**.

| | Live mode | Archive mode |
|---|---|---|
| Source | `simian serve` over MCP/SSE | a `simian-eval --out` run directory |
| Backend required | yes | **no** — static files |
| Audit events | tailed as they fire | read from the run's audit log |
| Primary view | active faults, cycle status | scorecard grid |
| Answers | "what is happening right now" | "what happened, and who caught it" |

This is worth stating as a design rule rather than an implementation note,
because it constrains something upstream: **the `simian-eval` run artifact must
be self-describing JSON that renders with no backend at all.** Not a database,
not a server API. That constraint is a gift rather than a cost — the same
property makes a run attachable to a CI job, e-mailable, diffable between two
commits, and openable by someone who has never installed Simian.

Two things make this cheaper than it sounds:

- **The two modes render the same events.** A live dashboard tailing the audit
  stream and a scorecard replaying a recorded one are the same renderer over the
  same vocabulary, differing only in where the events come from and whether they
  stop. The panels — active faults, topology, fault timeline — are reused
  verbatim; only the scorecard grid is net-new.
- **mast-web already has the seam.** `web/attach-core/replay.js` classifies
  replayed events against live ones on a shared renderer, with aggregate state
  updating from both while the transcript suppresses replay. That is a cutoff
  filter on live attach rather than an artifact loader, so it is not the whole
  answer — but the separation we need is already drawn in the code being ported.

The scorecard grid itself is the one genuinely new surface:

```
scenario                 efficacy   lookout   sre-agent   mast+lookout
fault-crashloop               ✓       1.00        1.00          1.00
fault-invoicing               ✓       0.00        0.50          1.00
latency-not-saturation        ✓       0.00        0.00          0.50
partition-one-way             ✗          —           —             —      not measured
pdb-gridlock                  ✓       1.00        0.00          1.00      ← agent below its own tool
```

Two rendering rules carry real weight and should not be lost in the port:

1. **A failed efficacy probe renders as *not measured*, never as zero.** A
   scenario whose fault did not land measured nothing about anyone, and showing
   it as a zero is the confident-wrong-number failure the whole eval substrate
   plan exists to prevent.
2. **The deterministic-detector column is a floor, not a peer.** A subject
   scoring below k8s-lookout on a row is failing to *use its tools*, which is a
   different bug from failing to diagnose. Flag it visually.

## Architectural pattern: thin client over MCP/SSE

Simian's existing MCP endpoint already exposes everything the UI needs to render:

- **Read-side** (already implemented, no changes needed):
  - `list_active_faults(namespace)` — current active-fault snapshot
  - `list_fault_catalog()` — available fault kinds per engine
  - `get_baseline(namespace)` — cached healthy-state snapshot
  - `get_topology(namespace)` — informer-backed workload list with envoy=true flags
  - `get_recent_faults(namespace, limit)` — recent applied+cleared history
- **Write-side** (already implemented):
  - `submit_fault(intent)` — LLM-translated manifest submission
  - `submit_manifest(manifest)` — deterministic-control submission
  - `clear_fault(fault_uid)` — pre-deadline clear
  - `establish_baseline(namespace, sut?)` — baseline capture
- **New** (needed for real-time observability):
  - `stream_audit_events(namespace?)` — SSE stream of audit events as they fire. Alternative: web UI polls `get_recent_faults` on interval (uglier but no server change).

```
┌──────────────┐   SSE stream        ┌──────────────────────┐
│  simian-web  │ ◄────────────────── │  simian serve        │
│  (browser)   │                     │  (:8081/sse)         │
│              │   fetch/POST        │                      │
│  Dashboard   │ ────────────────► │  - Executor          │
│  Submit form │                     │  - Autonomous loop   │
│  Live stream │                     │  - MCP tools         │
└──────────────┘                     │  - Audit log         │
                                     └──────────────────────┘
```

**What this is NOT:** a WASM agent-in-browser pattern. Simian's value is the persistent backend controller (audit log, RBAC, budget enforcement, in-process reaper). Browser is display + submit only, no agent state.

## What we lift from mast-web

`../mast-web` is a fully-shaped thin-client-over-attach-protocol web UI (vanilla JS + SSE + tiny Go static server). It targets a different agent (`mast`), but the shape is 90% reusable.

| Asset | Reuse plan |
|---|---|
| `web/index.html` — sidebar + main + status bar + modals layout | **Port verbatim.** Layout fits Simian too. |
| `web/styles.css` — TUI-aesthetic dark theme, Go palette, monospace | **Port verbatim.** Re-skin lightly to Simian branding. |
| `web/app.js` rendering surface | **Port with surgical replacement.** Keep: message rendering, streaming, sidebar, status bar, slash-command shape. Replace: the attach-protocol coupling → MCP/SSE coupling. |
| `cmd/mast-web-server/` — stdlib-only Go static server + reverse proxy | **Port verbatim.** Serves the SPA + proxies MCP endpoint with `FlushInterval=-1` for SSE. ~200 LOC. |
| Markdown + syntax highlight (`marked` + `marked-highlight` + `highlight.js` via CDN) | **Port verbatim.** |
| Deployment shape catalog (hosted SPA / container / agent `--ui` / self-host tarball) | **Reference, adapt.** Simian's default likely served-by-`simian serve` (agent-flag style), with the container option for team deployments. |

**What's simian-specific to build:**

- **Dashboard panels.** Active faults with countdown timers, cycle status card, topology tree, catalog picker. Not in mast-web (which is chat-shaped).
- **Fault submit form.** Two modes: intent (free-text → LLM translate) and deterministic (engine → kind → generated spec form based on the SpecTemplate from the catalog entry).
- **Live audit stream renderer.** Formatted events with severity coloring, expandable payloads, filter by event type.
- **Baseline diff view.** Cached baseline vs current topology — show workloads that have drifted (replica count differs, missing, extra).
- **Scorecard grid + run loader.** Scenarios × subjects, per-scenario drill-down, and a source switch that accepts a run directory (or a dropped JSON file) instead of an MCP URL. Not in mast-web.

## Stack decisions

Match mast-web's choices unless we have a strong reason to diverge:

| Decision | Choice | Rationale |
|---|---|---|
| Language | Vanilla JS for v0.1 | Same reasoning as mast-web — build pipeline is future work if scope grows. Revisit at ~3000 LOC. |
| Framework | None | Vanilla DOM + small helpers. |
| Build pipeline | None initially. `cp -R web/. dist/` + `go:embed` into the simian-web binary. |
| Connection transport | SSE for events, fetch for requests | MCP already SSE-based; no new transport. |
| Static server | Tiny Go binary, stdlib-only, ~200 LOC | Distroless-nonroot image, same as mast-web-server. |
| Auth | Reuse whatever `simian serve` exposes on the MCP endpoint (initially: none / trust in-cluster; later: bearer token, mTLS, IAP). | No new auth model in the web UI. |
| Markdown / syntax highlight | `marked` + `marked-highlight` + `highlight.js` via CDN | Same as mast-web. |

## Deployment options

Modeled on mast-web's four shapes. Simian's expected primary is #3 (agent-served), with #2 (container) as the team-deployment fallback.

1. **Hosted SPA** — `simian-web.pages.dev` or similar, points at operator-supplied `--mcp-url`. Zero install for operators. Best for cross-org / SaaS use, not day 1.
2. **Container image** — `ghcr.io/go-steer/simian-web:v0.1.0`. Team runs it as a K8s Deployment alongside `simian serve`. Optional server-side token injection for shared-backend / single-auth setups.
3. **Agent-served (recommended default)** — `simian serve --ui` embeds the SPA via `go:embed` and serves it at `:8081/ui/`. Same-origin auth, zero deploy overhead. Single artifact.
4. **Self-host tarball** — `simian-web-v0.1.0.tar.gz` unpacked into any static-file server (nginx, Cloud Storage, etc.). Advanced use.

Ship #3 first (single binary, easiest to try). Add #2 when a team asks. #1 + #4 are v1.0.0-era polish.

## Phased rollout

**v0.1 — Read-only dashboard.** Ship the SPA scaffold + agent-served option. Panels: active faults (with countdown), recent faults, topology, catalog list, baseline snapshot. Live update via SSE audit stream (new MCP tool `stream_audit_events`). No submit forms. Roughly 2-3 weeks including the new stream tool + Go static server + JS scaffold port.

**v0.2 — Manual submit.** Add the fault submit forms: intent (free-text) and deterministic (engine → kind → spec form generated from catalog SpecTemplate). Confirm-before-apply preview. Clear-fault buttons on active-fault cards. ~1-2 weeks on top of v0.1.

**v0.3 — Scorecard (archive mode).** Load a `simian-eval` run directory and render the grid + per-scenario drill-down. Depends on the run artifact existing, so it follows the eval rig rather than leading it — but the *shell* must be built with the source switch in mind from v0.1, because retrofitting a live-only client to read files is the expensive version of this. ~1-2 weeks once the artifact is stable.

**v0.4 — Polish + team-deploy.** Container image + Helm chart for team deployments. Optional auth passthrough. Baseline-diff view. Filter/search on audit stream. ~1-2 weeks.

**v1.0.0 candidate.** Freeze API + UI. Move `stream_audit_events` from experimental to stable. Document deployment shapes. Address any friction surfaced by first team deployments.

**Sequencing note.** v0.1–v0.2 (live) and v0.3 (archive) are independently useful and can be built in either order; the eval rig gates v0.3 and nothing gates v0.1. The one thing that must happen up front, in v0.1, is the **source abstraction** — every panel reads from an event source interface, and "live MCP/SSE" is one implementation of it. That is a day of design and it is the difference between one site and two.

## Out of v1

- **Plan-first approval gate.** Rendering LLM-authored plans for operator review/reject *before* execution. Requires backend changes to hold plans awaiting approval. Real product surface, but out of scope for v1 dashboard.
- **Multi-namespace tabbed view.** v0.1 assumes one-namespace-at-a-time. Multi-namespace pivots to a workspace model.
- ~~**Historical scenario replay.** Needs M5 scenario record export.~~ **In scope as of 2026-08-18** — this is archive mode, and the record it needed is the `simian-eval` run artifact.
- **Interactive terminal shell (`simian shell`).** If we build the web UI, the standalone REPL loses most of its case. Skip.

## Related work in the roadmap

- **Not on any current milestone.** This is polish/differentiator work, not on the M4-M6 critical path.
- **Best fit slot:** parallel to M5 (Scenario Export & Evaluation Substrate) or immediately before v1.0.0. M4 (Red Phone) has enough surface of its own; bundling would delay it.
- **Blocked on:** nothing structural for live mode. `stream_audit_events` MCP tool is the one net-new backend piece; ~1 day of work.
- **Archive mode is gated by the eval rig** — specifically the run artifact from `cmd/simian-eval`. See [`eval-substrate.md`]({{< relref "eval-substrate.md" >}}) §6.3 and §8.4.

## Open question resolved: one site or two

**Asked:** the live operator dashboard and the eval scorecard look like two products — a thing you watch while chaos is running, and a thing you read afterwards. Should they be two applications?

**Answered 2026-08-18: one.** They share a shell, a component set, an event vocabulary, a deployment story, and an aesthetic; they differ in where events come from and whether they stop arriving. Two apps would mean maintaining two ports of mast-web, two auth stories, and two answers to "where do I look at Simian" — to avoid writing one source abstraction.

The cost of the decision is real and worth naming: v0.1 must define the event-source interface even though it will only have one implementation for months. Accepted.

## Open questions

1. **SSE vs WebSocket for the audit stream.** SSE is simpler + matches the existing MCP transport. WebSocket would let the browser send events (e.g. "cancel this active fault"), but every write path already has a POST-based MCP tool, so SSE-in-only is likely sufficient. Default to SSE unless a real bidirectional need emerges.
2. **Do we want a single binary (`simian` with `--ui` flag) or a separate binary (`simian-web-server`)?** mast-web ships a separate binary for the container-deployment case. For agent-served, embedding into `simian serve` via `go:embed` + a `--ui` flag is one artifact for operators. Suggest: embed for the default, and if a team wants standalone, they can build a wrapper. Same binary either way.
3. **Auth story for v0.1.** MCP endpoint on `simian serve` is unauthenticated today (assumption: in-cluster or port-forwarded). Web UI inherits that assumption. Real auth (bearer, IAP, OIDC) is a backend concern; web UI just carries whatever headers/cookies the operator's browser presents. Deferred to when a team actually deploys shared.
