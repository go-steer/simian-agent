# `examples/` — opinionated starting points for new installs

Drop-in artifacts for common Simian Agent install patterns. None of
these are required; they exist so a new install doesn't have to
re-derive every value from scratch.

## Helm values overlays

| File | Purpose |
|---|---|
| [`values-baked-defaults.yaml`](values-baked-defaults.yaml) | Recommended Helm values for installs that want only the parts of Simian that have been verified end-to-end against a real cluster. Everything experimental (Envoy injection, autonomous mode, node-tier chaos) is explicitly off. **Maintained alongside the chart on the way to v1** — see the file's header for the maintenance contract. |

Use any overlay with:

```bash
helm install simian deploy/helm/simian -n simian-system \
  --create-namespace \
  -f examples/values-baked-defaults.yaml \
  -f my-install-overrides.yaml   # optional: your project, namespaces, etc.
```

## Manifest fragments (for `simian chaos --manifest`)

| File | Purpose |
|---|---|
| [`network-latency.json`](network-latency.json) | Inline `spec` for a NetworkChaos delay fault. Useful as a `--spec-file` reference (note: see the CLI bug noted in the acceptance results — currently use `--spec` with inline JSON). |
| [`network-latency-manifest.json`](network-latency-manifest.json) | A fully-formed `FaultManifest` JSON suitable for `simian chaos --manifest`. |

## What this directory is NOT

- A test fixture set — those live next to their packages as `*_test.go`.
- A scenario library — that's `pkg/sut/onlineboutique/manifests` and
  any future SUTs in `pkg/sut/<name>/`.
- A list of every supported configuration — the canonical reference
  for that is `deploy/helm/simian/values.yaml`. The overlays here
  are *opinionated subsets*, not exhaustive enumerations.
