---
title: "Deploying with Helm"
linkTitle: "Deploy"
weight: 60
description: "How to install the controller in-cluster via the Helm chart."
---

The Helm chart in `deploy/helm/simian/` runs the controller in-cluster. It pulls the image from `ghcr.io/go-steer/simian-agent`, published automatically by `.github/workflows/release.yml` on each `v*` tag push.

## Install patterns

```bash
# Default install (uses Chart.AppVersion as the image tag).
helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace

# Pin a specific published tag.
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set image.tag=v0.1.3-dev

# Enable the M3 in-controller SUT path (required for `simian sut deploy --use-controller`).
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set sutInController.enabled=true

# Recommended starting point: layer the "fully-baked-defaults" overlay
# on top of the chart defaults. Pins a known-verified image tag, tightens
# the executor safety policy, leaves experimental features off. See
# examples/values-baked-defaults.yaml for what each value is doing and
# the maintenance contract.
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --create-namespace \
    -f examples/values-baked-defaults.yaml
```

## Ad-hoc dev images

For dev builds without cutting a release tag, push your own image:

```bash
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
make image-push VERSION=mybranch IMAGE_NAME=myorg/simian-agent

helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set image.repository=ghcr.io/myorg/simian-agent \
    --set image.tag=mybranch
```

## Verifying the install

```bash
# Controller pod should be Ready in < 30s.
kubectl get pods -n simian-system

# MCP endpoint reachable via the service.
kubectl port-forward -n simian-system svc/simian-controller 8081:8081 &
curl -sS http://localhost:8081/sse -m 3 -o /dev/null -w "HTTP %{http_code}\n"
# Expected: HTTP 200 (the SSE endpoint streams; curl will -m 3 timeout).
```

See [Helm values reference]({{< relref "helm-values.md" >}}) for what each value does and the recommended overlay's [maintenance contract](https://github.com/go-steer/simian-agent/blob/main/examples/values-baked-defaults.yaml).
