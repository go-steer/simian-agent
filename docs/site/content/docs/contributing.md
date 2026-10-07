---
title: "Contributing"
linkTitle: "Contributing"
weight: 130
description: "How to file issues, structure PRs, and keep our chart values overlay honest."
---

The canonical contributor guide lives at [`CONTRIBUTING.md`](https://github.com/go-steer/simian-agent/blob/main/CONTRIBUTING.md) in the repo root. It covers:

- Reporting bugs and requesting features.
- The PR workflow: branch from `main`, conventional-commits messages, DCO sign-off.
- License headers (Apache 2.0; auto-checked by `dev/tools/lint-go`).
- Test discipline — unit tests live next to the code (`*_test.go`); integration tests are gated by build tags; end-to-end acceptance plans live at the repo root as `acceptance-mN.md`.
- The maintenance contract for [`examples/values-baked-defaults.yaml`](https://github.com/go-steer/simian-agent/blob/main/examples/values-baked-defaults.yaml) — every PR that adds a chart value, hardens an experimental feature, or surfaces a footgun MUST update that overlay in the same PR.

## Project layout (high-level)

| Directory | Purpose |
|---|---|
| `cmd/simian/` | CLI binary. Cobra subcommands: `arena`, `sut`, `serve`, `chaos`, `plan`, `evaluate`. |
| `pkg/` | Library packages: `arena/`, `audit/`, `catalog/`, `driver/{chaosmesh,networkpolicy,envoyfault}`, `executor/`, `lease/`, `llm/`, `loop/`, `mcp/`, `planner/`, `simian/`, `sut/`, `topology/`. |
| `api/v1alpha1/` | Typed CRDs / shared API structs. |
| `internal/testutil/` | Fakes and fixtures shared across test packages. |
| `deploy/` | Kubernetes manifests + Helm chart (`deploy/helm/simian/`). |
| `examples/` | Manifest fragments + the recommended Helm values overlay. |
| `dev/` | Local + CI tooling (run from here, don't reinvent). |
| `docs/` | This site's source (`docs/site/`) plus design / planning markdown. |
| `.github/workflows/` | Thin delegators to `dev/ci/presubmits/`. |

## Getting set up

```bash
git clone https://github.com/go-steer/simian-agent
cd simian-agent
make all                     # build + unit tests + lint
dev/tools/ci                 # full presubmit (format / vet / build / lint / mod-tidy / unit / vuln)
```

For the docs site itself:

```bash
cd docs/site
npm install                  # PostCSS + autoprefixer for the SCSS pipeline
hugo server                  # local preview at http://localhost:1313/simian-agent/
```

The Hugo site is built and deployed by [`.github/workflows/docs.yml`](https://github.com/go-steer/simian-agent/blob/main/.github/workflows/docs.yml) on every `main` push that touches `docs/site/**`.

## Versions and releases

Simian follows [semantic versioning](https://semver.org/). Before 1.0 that
means:

- **Minor** (0.2 → 0.3) for new features, or for anything that breaks an
  install or an upgrade: a chart value renamed or made required, a CLI flag
  changed, an audit event or field renamed or removed, an MCP tool's contract
  changed, a fault the executor used to accept now refused.
- **Patch** (0.2.0 → 0.2.1) for fixes only, safe to upgrade to without
  reading anything.
- **Release candidates** (`v0.2.0-rc.1`) for a release that is soaked before
  it is called done. A prerelease tag publishes its image but does not move
  `latest`, and gets no "latest" Release page, so nobody upgrades to it by
  accident.

The chart's `version` and `appVersion` move together with the release, and
`examples/values-baked-defaults.yaml` pins the same image (`verify-chart`
fails otherwise). A release is cut by a PR that bumps those, then a signed
`v…` tag on its merge; `release.yml` publishes the image. A stable release
gets a GitHub Release page whose notes list what changed by issue, and, for a
minor release, the upgrade notes: what an existing install must change.

The 0.1.x series did not follow this — features and breaking changes shipped
as patch releases. v0.2.0 is where it starts.

### What 1.0 means

1.0 promises that 1.x will not break an install. Before cutting it:

- the install surface is stable — chart values, CLI flags, audit event names
  and fields, MCP tool contracts — and documented as the compatibility
  promise;
- at least one team outside the project has run Simian for real;
- long soaks are routine and clean, on GKE and kind;
- upgrades between minor releases are documented and tested;
- there is a deprecation policy: something removed in 1.x is deprecated, with
  a warning, for at least one minor release first.
