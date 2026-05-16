# Contributing to simian-agent

Thanks for your interest in contributing! This file is the table of contents — most of the detail lives in [`dev/README.md`](./dev/README.md) and the [docs site](https://go-steer.github.io/simian-agent/).

By participating in this project you agree to abide by the [Code of Conduct](./CODE_OF_CONDUCT.md).

## Reporting bugs and requesting features

- **Bugs:** [open an issue](https://github.com/go-steer/simian-agent/issues/new) and include your OS / Go version, the cluster context (GKE / kind / k3d / etc.), the chaos engine in use (Chaos Mesh version), and the smallest set of steps that reproduces the problem. If the bug shows up against a specific cluster type or Chaos Mesh feature, mention which acceptance test (`acceptance-mN.md`) it relates to if any.
- **Feature requests:** check the [roadmap](https://go-steer.github.io/simian-agent/docs/roadmap/) and the [open milestones](https://github.com/go-steer/simian-agent/milestones) first — your idea may already be planned. If not, file an issue with the use case (what you're trying to do) before the proposed solution.
- **Questions / discussion:** [GitHub Discussions](https://github.com/go-steer/simian-agent/discussions).

## Pull requests

### Before you start

For anything beyond a typo fix or one-line bug, open an issue first so we can agree on the approach. PRs that are aligned upfront merge faster than ones that surface a design disagreement at review time.

For substantive features, the project follows a milestone-based development model — each milestone gets an `acceptance-mN.md` plan written before the work starts, and an entry added to the [roadmap](https://go-steer.github.io/simian-agent/docs/roadmap/) when it lands. If your change is large enough to warrant a milestone, propose the scope in an issue first.

### Workflow

1. Fork and create a short-lived feature branch off `main` (e.g. `feat/m4-red-phone`, `fix/chaos-leak`, `docs/install-snippet`).
2. Make your change. Keep the diff focused; unrelated cleanup belongs in a separate PR.
3. Run the full local CI before pushing:
   ```bash
   dev/tools/ci
   ```
   This is the same script that runs in GitHub Actions — green locally means green remotely. See [`dev/README.md`](./dev/README.md) for the full layout and how to add new checks.
4. Open the PR against `main`. CI runs on the PR; the four required status checks (`test`, `lint`, `go mod tidy is clean`, `govulncheck`) gate the merge. Docs-only PRs satisfy these checks via a companion no-op workflow without running the full Go pipeline.

### Commit messages — Conventional Commits

Subject lines follow [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` — user-visible new functionality
- `fix:` — user-visible bug fix
- `docs:` — documentation only
- `test:` — tests only
- `refactor:` — code change that's neither a feature nor a fix
- `chore:` / `build:` / `ci:` — repo plumbing

Optional scope in parens: `feat(planner): ...`, `fix(executor): ...`. Keep the subject under ~70 chars; put detail in the body explaining *why* and what verification you did.

### Developer Certificate of Origin (DCO)

All commits must be **signed off** under the [Developer Certificate of Origin](https://developercertificate.org/). The DCO is a lightweight assertion that you wrote the patch (or have the right to submit it under the project's Apache-2.0 license) — it's a `Signed-off-by:` trailer in the commit message, not a cryptographic signature.

Sign off by passing `-s` to `git commit`:

```bash
git commit -s -m "feat(planner): add depends_on layering"
```

…which appends:

```
Signed-off-by: Your Name <you@example.com>
```

The name and email must match your `git config user.name` / `user.email`. If you forget, amend with `git commit --amend -s` (single commit) or rebase with `-x 'git commit --amend -s --no-edit'` (multiple).

### License headers

Every source file carries the full Apache 2.0 header attributed to Google LLC:

```
// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
```

`golangci-lint` enforces this on `.go` files automatically via the `goheader` linter. For new shell, YAML, or Python files (and the repo-root `Dockerfile` / `Makefile`), run `dev/tools/add-license-headers` once — it's idempotent and normalizes any existing header (including the older SPDX-shorthand variant) to the canonical form. See [`dev/README.md`](./dev/README.md#license-headers) for the full rules.

### Tests

- Unit tests live next to the code (`*_test.go`).
- Integration tests are gated by build tags. `pkg/llm/gemini/gemini_integration_test.go` (build tag `integration`) hits a real Vertex / Gemini endpoint and is skipped by the default `go test ./...` run; invoke explicitly with `go test -tags=integration ./pkg/llm/gemini/...` once your `GOOGLE_CLOUD_PROJECT` / `GEMINI_API_KEY` env is set.
- End-to-end acceptance plans live at the repo root as `acceptance-mN.md`. Each milestone gets its own plan; the plan is written before the work starts and verified before tagging.
- A new feature without a test is not done. A new bug fix without a regression test makes it easy for the bug to come back.

### Keep `examples/values-baked-defaults.yaml` honest

On the way to v1, `examples/values-baked-defaults.yaml` is the single source of truth for "what should I turn on in production right now?". Every PR that:

1. Adds a new chart value or CLI flag, OR
2. Hardens an experimental feature so it's safe to leave on, OR
3. Surfaces a regression / footgun that warrants flipping a previously-on feature off

MUST update this overlay in the same PR. The "Hardening log" comment block at the bottom of the overlay tracks each major decision and the chart version that hardened (or un-hardened) it; append to that log when you touch the corresponding value. Goal: when v1 ships, this overlay should look like a near-empty file.

## Project layout

- `cmd/simian/` — CLI binary (`simian arena`, `simian sut`, `simian chaos`, `simian plan`, `simian serve`).
- `pkg/` — library packages: `arena/`, `audit/`, `catalog/`, `driver/`, `executor/`, `lease/`, `llm/`, `loop/`, `mcp/`, `planner/`, `simian/`, `sut/`, `topology/`.
- `api/v1alpha1/` — typed CRDs / shared API structs.
- `internal/testutil/` — fakes and fixtures shared across test packages.
- `deploy/` — Kubernetes manifests + Helm chart (`deploy/helm/simian/`).
- `examples/` — minimal usage / driver examples.
- `dev/` — local + CI tooling (run from here, don't reinvent).
- `docs/site/` — Hugo + Docsy source for the published documentation site at <https://go-steer.github.io/simian-agent/>. All design notes live here as content pages; the site is the single source of truth.
- `.github/workflows/` — thin delegators to `dev/ci/presubmits/`.

For deeper context on conventions and gotchas, read the project [README.md](./README.md).

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](./LICENSE).
