# Sprint 1 + Sprint 2 execution log

Autonomous execution of the punch-list items prioritized in the prior chat. Each section captures the work done, the critical decisions made (highlighted), and links to the resulting PR.

**Start:** 2026-05-16
**Branches:** one per item, all targeting `main`.
**Decision posture:** for any open question, default to the recommendation given in the prior planning exchange.

---

## Sprint 1 — clear the deck

### Item #5 — Verify `make image-push` for arbitrary dev tags ✅

| | |
|---|---|
| Branch | N/A (verification only) |
| PR | none |
| Status | **PASS** |
| Effort | 15 min including the ~140s docker build |

**What was verified:**
1. `make -n image-push VERSION=verify-test IMAGE_NAME=myorg/simian-agent` dry-runs to:
   ```
   docker build -t ghcr.io/myorg/simian-agent:verify-test .
   docker push ghcr.io/myorg/simian-agent:verify-test
   ```
   — matches what the README documents. `IMAGE_REGISTRY`, `IMAGE_NAME`, `VERSION` overrides all work.
2. Actual `make image VERSION=verify-test` (no push) builds cleanly against current main. Final layer: `gcr.io/distroless/static`, simian binary at `/usr/local/bin/simian`. Digest `sha256:821f3b85aa3e5c0ad192566cbc416a6ba6c0eeb3e65a588964760a3748dfdfc7`.
3. `docker run --rm ghcr.io/go-steer/simian-agent:verify-test --help` runs the CLI; all subcommands (`arena`, `chaos`, `evaluate`, `plan`, `serve`, `sut`) are present.
4. Image deleted after smoke test (`docker image rm`).

**🟡 Side-finding folded into Sprint 1 #2:** `simian chaos --engine` help text still reads `"Chaos engine (chaos-mesh|litmus)"`. Stale — should include `network-policy` and `envoy-fault`. Falls into the same "small CLI cleanup" bucket as the `--spec-file` bug, so I'm folding it into PR #2 to keep the PR-count down.

**Critical decisions made:**
- **Skipped actually pushing** to ghcr (no auth set up for a throwaway test tag; the build + run confirms the publishable artifact is sound). Push step is `docker push <tag>` which only depends on docker being logged in — proven separately every time the release workflow fires.
- **Did not open a fix PR** since nothing is broken.

---

### Item #2 — Fix `--spec-file` CLI flag binding bug ✅

| | |
|---|---|
| Branch | `fix/cli-spec-file-binding` |
| PR | [#33](https://github.com/go-steer/simian-agent/pull/33) (merged) |
| Status | **DONE** |
| Effort | ~45 min |

**What shipped:**
- Dedicated `specFile string` variable + separate Cobra binding.
- `loadSpec(specJSON, specFile, fromStdin)` reads the file when `specFile` is set.
- Drops the `@`-prefix hack (only existed because of the binding bug).
- Strips the now-unused `"strings"` import.
- New `cmd/simian/chaos_test.go` with 6 test cases covering inline JSON, file path (regression guard), empty input, overlapping-inputs rejection (all 3 pairwise combos), missing file, invalid JSON.
- Deleted the bug entry from `known-limitations.md`; rewrote the warning in `cli-reference.md` as a one-line note that the three input flags are mutually exclusive.
- Folded in the side-finding: `--engine` help text updated from `(chaos-mesh|litmus)` to `(chaos-mesh|network-policy|envoy-fault)`. `--kind` and `--api-version` help tightened to mention the per-engine overrides.

**Critical decisions made:**
- **Reject overlapping inputs rather than pick one.** When the user sets two of `--spec`/`--spec-file`/`--stdin-spec`, the CLI returns an error instead of silently picking one. Rationale: the original bug went unnoticed for weeks precisely because the silent-fallback behavior masked it. A loud error is the right default for a tool that produces real cluster effects.
- **Removed the `@`-prefix hack from `loadSpec` rather than keeping it as a vestigial alternative path.** Nobody documents it; the new `--spec-file` flag covers the same use case more clearly. Keeping the dead code for "compatibility" with operators who happened to discover the hack would have been YAGNI noise.
- **Used a flat `loadSpec(specJSON, specFile, fromStdin)` signature rather than a struct.** Three params, easy to read at the call site. A struct buys nothing here and would have made the test cases noisier.
- **No CLI flag rename.** Considered `--spec-file` → `--spec-from-file` for clarity, but `--spec-file` is what the README and prior conversation already use; renaming would just orphan operator muscle memory.

---

## Sprint 2 — Envoy probe-rewriting

### Item #1a — `ExcludePorts` knob (cheap alternative) ✅

| | |
|---|---|
| Branch | `feat/envoy-inject-exclude-ports` |
| PR | [#34](https://github.com/go-steer/simian-agent/pull/34) (merged) |
| Status | **DONE** |
| Effort | ~2 hours (faster than estimated — minimal scope) |

**What shipped:**
- `InjectOptions.ExcludePorts []int` — programmatic knob for embedders.
- `DeployOptions.EnvoyExcludePorts []int` + `--envoy-exclude-port` CLI flag (`IntSliceVar`, repeatable).
- `sut.EnvoyExcludePortsProvider` interface — optional method a SUT may implement to declare its probe ports at registration time.
- `simian.chaos/envoy-exclude-ports` pod-template annotation — per-Deployment granular escape hatch.
- All four sources merged via `uniqueSortedPorts` for stable ordering + dedup.
- `buildIptablesScript` emits RETURN rules for excluded ports BEFORE the REDIRECT rules — nf_tables walks in order and short-circuits on a match.
- 4 new tests: rule ordering invariant, multi-layer merging, exclude-only short-circuit guard, annotation parser (6 input shapes).
- `known-limitations.md` gains a "Cheap-escape-hatch" subsection with CLI + annotation + interface examples + the probe-port-equals-service-port trade-off callout.

**Critical decisions made:**
- ✅ **Separate `EnvoyExcludePortsProvider` interface, NOT folded into `EnvoyFaultPortsProvider`.** As predicted in the plan. They're conceptually different — one is "what to intercept", the other is "what NOT to intercept". SUTs that need only one shouldn't have to implement the other.
- **Did NOT update Online Boutique's SUT to declare its probe ports.** Reasoning: Online Boutique's probe ports == service ports for most of its 12 workloads. Adding them to `EnvoyExcludePorts` would silently disable fault injection on those workloads — exactly the trade-off this PR documents but doesn't want to ship as the default. The right move is to keep Online Boutique unaltered and rely on the full probe-rewriter (1b) to fix it properly. Documented in the PR body.
- **`IntSliceVar` for the CLI flag** rather than a comma-separated `StringVar`. Cobra handles `--envoy-exclude-port=80 --envoy-exclude-port=443` AND `--envoy-exclude-port=80,443` natively with IntSlice; no manual parsing.
- **Silent-skip of invalid entries in the annotation parser.** Rationale: this annotation lives on pod templates that may be hand-edited by operators; a typo (e.g. `"80, 443x"`) shouldn't fail the entire SUT deploy. Logged with a comment explaining the choice.
- **Did NOT flip the chart default.** That's gated on 1b. This PR ships as additive — `sutInjection.envoyFaults` stays `false`.

---

### Item #1b — Full Istio-style probe rewriter ✅

| | |
|---|---|
| Branch | `feat/envoy-probe-rewriter` |
| PR | [#35](https://github.com/go-steer/simian-agent/pull/35) (merged) |
| Status | **DONE** (code; in-cluster verification is a separate follow-up) |
| Effort | ~3 hours code + tests, well under the ~1-day pragmatic budget |

**What shipped:**

- `cmd/simian-envoy-agent/` — new Go binary, ~330 lines including tests. Reads `metadata.annotations` from a downward-API volume at startup, populates a probe registry, serves HTTP on port 15021. For each `GET /app-health/<container>/<kind>`, looks up the stashed probe and executes it against `127.0.0.1`. gRPC via `google.golang.org/grpc/health/grpc_health_v1`. HTTP via stdlib. TCP via `net.Dial`. Returns 200 / 503 to kubelet.
- `pkg/sut/envoy/probe.go` — shared types (`StashedProbe`, `ProbeKind`) + annotation key helpers + `ExecuteProbe`. Used by both the injector and the agent.
- `pkg/sut/envoy/inject.go` — new `rewriteProbes()` mutates each workload container's probes to httpGet against the agent; preserves timing fields (initialDelaySeconds, etc.); stashes original spec as `simian.chaos/probe-<container>-<kind>` annotation.
- Agent sidecar auto-added when at least one probe was rewritten; auto-skipped when no container had a rewritable probe.
- Downward-API annotations volume auto-mounted on the same condition.
- Agent's listener port (15021) auto-added to iptables exclude list — prevents the agent's own probe traffic from being redirected back to Envoy.
- `InjectOptions.AgentImage` (default `ghcr.io/go-steer/simian-agent:latest`) + `DisableProbeRewrite` opt-out.
- Dockerfile builds both binaries (`simian` and `simian-envoy-agent`) into the same image; injected Deployments override `container.command` to invoke the agent.
- 10 new tests: probe types round-trip, all 3 probe-kind executors, rewriter end-to-end, opt-out path, exec-probe pass-through, annotation parser, registry loader, HTTP handler 200/404 paths.

**Critical decisions made:**

- ✅ **Separate Go binary, NOT Envoy Lua/WASM filter.** As predicted in the plan. Envoy's Lua filter could theoretically do HTTP→HTTP translation but can't synthesize a gRPC health-check protocol call. The Go binary is ~330 lines of code we control vs. wrestling with Envoy's filter ecosystem.
- ✅ **Single shared image** for `simian` (controller) + `simian-envoy-agent` (sidecar). One Dockerfile, one CI publish, one Helm version to track. The image-size delta is negligible (~10 MB for the agent). Injected Deployments select the agent via `command: [/usr/local/bin/simian-envoy-agent]`. Alternative considered: separate image. Rejected — extra ops surface for no real win.
- **Exec probes deferred.** Supporting exec probes would require either (a) the agent shelling out to a script that lives in another container's filesystem (privileged), or (b) requiring SUTs to colocate the script in a shared volume. Neither is clean. v1 returns `ok=false` from `StashProbe` for exec probes; the injector leaves them in place rather than rewriting to a path the agent can't serve. Acknowledged in the PR body as a known limitation.
- **Timing fields preserved on rewrite.** `initialDelaySeconds`, `periodSeconds`, `failureThreshold`, `timeoutSeconds` etc. stay on the rewritten httpGet probe — they apply to the synthetic kubelet↔agent probe, which is the right semantic (vs. moving them to the stashed probe and having the agent re-implement kubelet's retry loop, which would be wrong).
- **Auto-skip when nothing to rewrite.** If the only probe in the pod is an exec probe (or there are no probes at all), the agent sidecar is NOT added — no point running a sidecar that has nothing to serve. The `rewriteProbes()` function returns `true` only if at least one probe was successfully rewritten.
- **Did NOT flip the chart default in this PR.** That's a follow-up after in-cluster verification. The PR ships as additive.
- **`DefaultAgentImage = "ghcr.io/go-steer/simian-agent:latest"`** — floating tag. Production operators should pin to a verified tag via `InjectOptions.AgentImage` override. Considered "auto-match the controller's own image" (better default), but pulling that through the call chain (controller → MCP → sut.Manager → envoy.Inject) is more plumbing than this PR's scope justifies. Phase 4 follow-up.
- **Lint cleanup:** Refactored `main()` into `run() int` + `os.Exit(run())` to satisfy `gocritic exitAfterDefer`. Added `#nosec G703` on the operator-controlled `os.ReadFile(path)` call with a justification comment.

---

## Cleanup at end

After all 4 PRs merge:

- Tag bump (`v0.1.4-dev`?) to ship the new agent image + chart updates.
- Move chart default `sutInjection.envoyFaults: true` once #1b lands and is verified.
- Update `examples/values-baked-defaults.yaml` "Hardening log" with the chart version that flipped Envoy back on.
- Delete or archive this execution log if it's no longer useful (or leave as historical).

---

## Summary at end of sprints

**Status: both sprints complete.** Four PRs shipped, all merged to `main`:

| PR | Item | Status |
|---|---|---|
| (none) | #5 — verify `make image-push` | ✅ verified, no fix needed |
| [#33](https://github.com/go-steer/simian-agent/pull/33) | #2 — fix `--spec-file` CLI bug + stale `--engine` help | ✅ merged |
| [#34](https://github.com/go-steer/simian-agent/pull/34) | #1a — `ExcludePorts` knob (cheap workaround) | ✅ merged |
| [#35](https://github.com/go-steer/simian-agent/pull/35) | #1b — full probe-rewriter sidecar (architectural fix) | ✅ merged |

**Total effort:** ~5 hours across all 4 items, well under the original ~1.5 days projected. Two factors: (1) #5 turned out to be pure verification + zero fix, (2) #1b ended up ~330 lines of well-scoped Go vs. the "~1 week comprehensive" upper bound I'd estimated.

**Open follow-ups** (deferred, not blockers):

1. **In-cluster verification of the probe rewriter.** PR #35 ships with unit tests only. Should re-run the M3 acceptance against `boutique-m3` with `--no-envoy-faults=false` to confirm Online Boutique's 12 deployments all come up Ready (the original failure mode this fix is supposed to address).
2. **Chart default flip.** Once #1 above passes, a tiny PR can set `sutInjection.envoyFaults: true` in the chart values. Then `examples/values-baked-defaults.yaml` "Hardening log" gets an entry recording the chart version that did the flip.
3. **Tag bump** (`v0.1.4-dev`) to publish a fresh image carrying the new `simian-envoy-agent` binary. Helm installs pointing at `latest` will get it automatically; pinned installs need the explicit bump. Recommend tagging only after #1 above passes.
4. **`DefaultAgentImage` auto-match the controller's own image.** Currently hardcoded to `ghcr.io/go-steer/simian-agent:latest`. Cleaner would be to plumb the controller's own image tag through the call chain so injected pods always match. Punt to Phase 4 since it's plumbing, not correctness.
5. **Exec probe support.** Out of scope for v1 of the rewriter; rare; would require either a privileged exec or shared-volume convention. Add when a real SUT needs it.

**Files added/modified across all 4 PRs** (excluding tests/docs):

| Path | What |
|---|---|
| `cmd/simian-envoy-agent/main.go` | New binary — probe-rewriter sidecar (#35) |
| `cmd/simian/chaos.go` | `--spec-file` binding fix + stale `--engine` help (#33) |
| `cmd/simian/sut.go` | `--envoy-exclude-port` flag (#34) |
| `pkg/sut/envoy/inject.go` | ExcludePorts plumbing (#34) + probe rewriter (#35) |
| `pkg/sut/envoy/bootstrap.go` | New constants for agent sidecar + annotations volume (#34, #35) |
| `pkg/sut/envoy/probe.go` | New — StashedProbe + ExecuteProbe (#35) |
| `pkg/sut/manager.go` | `EnvoyExcludePorts` field on DeployOptions (#34) |
| `pkg/sut/sut.go` | `EnvoyExcludePortsProvider` interface (#34) |
| `Dockerfile` | Build + ship the agent binary alongside the controller (#35) |
| `go.mod` | grpc promoted from indirect to direct (#35) |
| `docs/site/content/docs/known-limitations.md` | Removed `--spec-file` bug entry; added the cheap-escape-hatch subsection (#33, #34) |
| `docs/site/content/docs/cli-reference.md` | Updated spec-input flag description (#33) |

**Critical-decision posture honored:** every open question was resolved using my prior recommendation as documented in the planning exchange. Logged inline in each item's "Critical decisions made" subsection above. No decisions deferred for user input mid-execution.
