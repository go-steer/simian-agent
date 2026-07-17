---
title: "Web UI design"
linkTitle: "Web UI (design)"
weight: 60
description: "Design doc for a browser-based dashboard + manual-submit surface over `simian serve`."
---

> **Status:** draft, 2026-07-17. Not implemented. Companion to [`design.md`]({{< relref "design.md" >}}) and the [Roadmap]({{< relref "roadmap.md" >}}).

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

## Two customers, one UI

| Customer | Uses UI for | Views they need |
|---|---|---|
| **Manual operator** (game-day driver, incident responder) | Discover + submit a fault, watch its effect, clear it | Catalog picker → spec form with hints → confirm-and-apply → status watch → clear button |
| **Autonomous observer** (SRE on-call, resilience-team lead) | Watch what the running loop is doing, spot-check safety, take over if needed | Live audit stream, active faults with countdowns, current cycle progress, recent plans with rationale, topology + baseline diff, "pause" and "clear all" affordances |

Both share the same live data (audit stream, active-faults, topology, catalog, baseline). Differ mostly in which panels are prominent + the presence of a submit form. Ship them as tabs (or side-by-side panels), not separate apps.

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

**v0.3 — Polish + team-deploy.** Container image + Helm chart for team deployments. Optional auth passthrough. Baseline-diff view. Filter/search on audit stream. ~1-2 weeks.

**v1.0.0 candidate.** Freeze API + UI. Move `stream_audit_events` from experimental to stable. Document deployment shapes. Address any friction surfaced by first team deployments.

## Out of v1

- **Plan-first approval gate.** Rendering LLM-authored plans for operator review/reject *before* execution. Requires backend changes to hold plans awaiting approval. Real product surface, but out of scope for v1 dashboard.
- **Multi-namespace tabbed view.** v0.1 assumes one-namespace-at-a-time. Multi-namespace pivots to a workspace model.
- **Historical scenario replay.** Needs M5 scenario record export. Meaningful after M5 ships.
- **Interactive terminal shell (`simian shell`).** If we build the web UI, the standalone REPL loses most of its case. Skip.

## Related work in the roadmap

- **Not on any current milestone.** This is polish/differentiator work, not on the M4-M6 critical path.
- **Best fit slot:** parallel to M5 (Scenario Export & Evaluation Substrate) or immediately before v1.0.0. M4 (Red Phone) has enough surface of its own; bundling would delay it.
- **Blocked on:** nothing structural. `stream_audit_events` MCP tool is the one net-new backend piece; ~1 day of work.

## Open questions

1. **SSE vs WebSocket for the audit stream.** SSE is simpler + matches the existing MCP transport. WebSocket would let the browser send events (e.g. "cancel this active fault"), but every write path already has a POST-based MCP tool, so SSE-in-only is likely sufficient. Default to SSE unless a real bidirectional need emerges.
2. **Do we want a single binary (`simian` with `--ui` flag) or a separate binary (`simian-web-server`)?** mast-web ships a separate binary for the container-deployment case. For agent-served, embedding into `simian serve` via `go:embed` + a `--ui` flag is one artifact for operators. Suggest: embed for the default, and if a team wants standalone, they can build a wrapper. Same binary either way.
3. **Auth story for v0.1.** MCP endpoint on `simian serve` is unauthenticated today (assumption: in-cluster or port-forwarded). Web UI inherits that assumption. Real auth (bearer, IAP, OIDC) is a backend concern; web UI just carries whatever headers/cookies the operator's browser presents. Deferred to when a team actually deploys shared.
