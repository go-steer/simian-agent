# dev/

Build- and test-tooling. Same scripts power both local development and
GitHub Actions CI, so a green local run is the same green run as remote.

## Quickstart

```bash
# Run every CI check locally (fast-fail order).
dev/tools/ci

# Run all checks even after a failure (collect every problem at once).
dev/tools/ci --keep-going

# Auto-fix formatting (gofmt + goimports).
dev/tools/fix-go-format
```

Missing tools (`golangci-lint`, `goimports`, `govulncheck`) auto-install
into `$GOBIN` (or `$(go env GOPATH)/bin`) on first use. No setup needed
beyond a Go toolchain.

## Layout

```
dev/
├── tools/                 # entry points users run locally
│   ├── ci                 # aggregator — runs every check below
│   ├── vet                # go vet ./...
│   ├── build              # go build ./...
│   ├── test-unit          # go test -race -coverprofile
│   ├── lint-go            # golangci-lint (auto-installs v2.12.1)
│   ├── verify-go-format   # gofmt -s + goimports check (read-only)
│   ├── fix-go-format      # gofmt -s -w + goimports -w (auto-fix)
│   ├── verify-mod-tidy    # `go mod tidy` clean check
│   ├── verify-vuln        # govulncheck ./...
│   ├── add-license-headers # bulk-applier for Apache 2.0 headers
│   ├── cluster-up         # kind + Calico + Chaos Mesh   (`make cluster`)
│   ├── cluster-down       # tear it down                 (`make cluster-down`)
│   ├── test-e2e           # go test -tags e2e ./test/... (`make e2e`)
│   ├── eval-lookout       # score the lookout pack       (`make eval-lookout`)
│   ├── common.sh          # shared bash helpers (ensure_tool, run_step)
│   └── .golangci.yml      # linter config
├── kind/
│   └── cluster.yaml       # kind topology + the CNI decision
└── ci/
    └── presubmits/        # thin delegators called by .github/workflows/ci.yml
        ├── vet            # → dev/tools/vet
        ├── build          # → dev/tools/build
        ├── test-unit      # → dev/tools/test-unit
        ├── lint-go        # → dev/tools/lint-go
        ├── verify-go-format
        ├── verify-mod-tidy
        └── verify-vuln
```

## Adding a check

1. Drop a new script under `dev/tools/<name>` (executable, `set -euo pipefail`,
   sources `common.sh`).
2. Add it to the `STEPS` array in `dev/tools/ci`.
3. Add a one-line delegator under `dev/ci/presubmits/<name>` that
   `exec`s the tool script.
4. Reference the presubmit from `.github/workflows/ci.yml`.

That's it — the delegator pattern means the GitHub workflow never has
to know what the check actually does.

## CI on PRs

Open a PR against `main` from a short-lived feature branch (e.g.
`feat/m4-red-phone`, `fix/chaos-leak`). CI runs on the PR; merging is
gated on the four required status checks.

For this to actually gate merges, the repo's branch protection on
`main` must require these checks (settings → branches → main):

- `test`
- `lint`
- `go mod tidy is clean`
- `govulncheck`

Docs-only PRs (`**/*.md`) are handled by the companion `ci-docs.yml`
workflow, which emits the same four check names trivially-green so
branch protection is satisfied without running the full Go pipeline.

## The e2e cluster

The presubmits above are hermetic — no cluster, no Docker, no network. The
live-cluster tier is separate:

```bash
make cluster        # kind + Calico + Chaos Mesh (~4 min cold)
make e2e            # go test -tags e2e ./test/...
make cluster-down
```

Needs `kind` **v0.32.0 or newer**, `kubectl`, `helm`, and a container runtime
on `PATH`:

```bash
go install sigs.k8s.io/kind@v0.32.0
```

`make cluster` checks the version before it builds anything. Older kind is
rejected rather than warned about, because the failure it causes is invisible
until the end: the cluster comes up healthy, Calico and Chaos Mesh install, and
then `kind load docker-image` fails with `unknown containerd config version: 4`
because the pinned node image runs containerd v2. That is the step that puts
the image under test into the cluster, so everything downstream is untestable
while everything upstream looks fine. CI pins the same version, so CI never
sees it.

Credentials are written to `.kube/e2e.yaml` in the work tree, never merged
into `~/.kube/config`: Simian creates namespaces, binds RBAC, and kills pods,
so the file it is handed must not be able to name a real cluster. See the
package comment in `internal/kindcluster` for the four guards that enforce
that, and `dev/kind/cluster.yaml` for why the default CNI is not used.

`make cluster` refuses to adopt an existing cluster of the same name. If a run
was killed before teardown, clear the leftover with
`go run ./internal/kindcluster/kindctl reap`.

## Scoring the rig against a real detector

`make e2e` asserts the substrate works. The next tier asserts the whole
pipeline does — inject, gate on efficacy, ask a subject, score:

```bash
make cluster
make eval-lookout                                    # the whole pack, ~6 min
make eval-lookout ARGS="--only lookout-image-pull"   # one scenario
```

The subject is [k8s-lookout](https://github.com/go-steer/k8s-lookout), pinned
to a released version in `dev/tools/eval-lookout` and built from the module
proxy — the two repos share no Go dependency in either direction, and argv and
text are the whole contract between them.

That it is a *detector* rather than an agent is the point. An agent's score
moves for three reasons: the fault, the agent, and sampling noise. A detector
returns the same findings for the same namespace, so there is no third term,
and two runs of one scenario that disagree are a bug in Simian — a fault that
landed late, an efficacy gate that passed early, an arena that leaked into the
next scenario. That is a test no agent subject can produce.

What the run fails on is the **efficacy rate**, not the score. Every fault in
the pack must become observable before its probes give up. Recall is printed
and not gated: it is a measurement of the detector, and a detector that got
worse at diagnosing a namespace is k8s-lookout's bug to fix.

CI runs both tiers as the `e2e-kind` workflow on push to `main`, not on PRs —
it pulls Calico and the Chaos Mesh chart from the network, and a required check
that can fail on someone else's CDN taxes every PR. It is not one of the four
gating checks. Push gets a three-scenario eval smoke; a weekly cron and manual
dispatch get the whole pack, with `run.json` kept as an artifact so the
detector baseline can be compared across weeks.

## License headers

Every source file carries the full Apache 2.0 header at the top,
attributed to Google LLC:

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

(`#`-prefixed for shell, YAML, Python, `Dockerfile`, and `Makefile`.)
The `goheader` linter inside `dev/tools/lint-go` enforces this on every
`.go` file — CI fails if a new Go source is missing it. For shell, YAML,
Python, and the repo-root build files, run `dev/tools/add-license-headers`
after creating new ones; the script is idempotent and normalizes any
existing header (including the older SPDX-shorthand variant) to the
current canonical form.

Files explicitly skipped by the bulk-add tool:

- `pkg/sut/onlineboutique/manifests/*.yaml` — third-party (Google Cloud
  Online Boutique upstream); a Google copyright header here would be a
  misattribution.
- `deploy/**/*.yaml` and Helm chart templates — Kubernetes manifests /
  config artifacts, not project source.

## Pinned tool versions

| Tool          | Version    | Source                                                     |
|---------------|------------|------------------------------------------------------------|
| golangci-lint | v2.12.1    | `dev/tools/lint-go` (`GOLANGCI_LINT_VERSION` env var)      |
| goimports     | latest     | `dev/tools/fix-go-format`, `dev/tools/verify-go-format`    |
| govulncheck   | latest     | `dev/tools/verify-vuln`                                    |

Bump deliberately — new linter releases can introduce findings that
block CI. When you bump golangci-lint, run `dev/tools/lint-go` locally
first to fix anything new before pushing.
