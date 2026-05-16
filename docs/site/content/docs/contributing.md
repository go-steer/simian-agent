---
title: "Contributing"
linkTitle: "Contributing"
weight: 110
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
