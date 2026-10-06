---
title: "Deploying with Helm"
linkTitle: "Deploy"
weight: 20
description: "How to install the controller in-cluster via the Helm chart."
---

The Helm chart in `deploy/helm/simian/` runs the controller in-cluster. It pulls the image from `ghcr.io/go-steer/simian-agent`, published automatically by `.github/workflows/release.yml` on each `v*` tag push.

## Install patterns

```bash
# Default install (uses Chart.AppVersion as the image tag).
helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace

# Pin a specific published tag.
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set image.tag=0.1.14

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

## From an empty GKE cluster

The chart installs the controller; it does not install what the controller
needs around it. On a fresh Standard GKE cluster that is four more steps, in
this order. Autopilot cannot run Chaos Mesh's `chaos-daemon` — see
[GKE bring-up]({{< relref "gke-bring-up.md" >}}).

The `network-policy` engine needs the cluster to enforce NetworkPolicies, and
a Standard cluster created without `--enable-dataplane-v2` (or
`--enable-network-policy`) does not: the policy is created and nothing is
blocked. Simian's efficacy gate notices and rolls the fault back
(`probe-failed`), so nothing is misreported, but every such fault is wasted.
Create the cluster with `--enable-dataplane-v2` if you want that engine.

### 1. Chaos Mesh

```bash
helm upgrade --install chaos-mesh \
    https://charts.chaos-mesh.org/chaos-mesh-2.8.4.tgz \
    -n chaos-mesh --create-namespace \
    --set chaosDaemon.runtime=containerd \
    --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
    --set dnsServer.create=true \
    --wait
kubectl -n chaos-mesh get pods   # one chaos-daemon per node you will inject into
```

2.8.4 is the version the kind e2e suite pins (`internal/kindcluster/install.go`)
and the one the GKE trials ran. The two `chaosDaemon` flags matter: GKE nodes
run containerd, and the Chaos Mesh chart defaults to Docker's runtime and
socket. `dnsServer.create=true` installs the DNS server DNSChaos works through;
without it a DNSChaos applies cleanly and changes nothing. Never mark the
`chaos-mesh` namespace eligible.

### 2. Vertex AI access from the pod

The chart defaults to Vertex (`llm.vertex.enabled: true`) through the pod's own
identity, which on GKE means Workload Identity:

```bash
# Once per cluster: enable Workload Identity (pool <project>.svc.id.goog).
# Existing node pools also need --workload-metadata=GKE_METADATA.
gcloud container clusters update "$CLUSTER" --region "$REGION" \
    --workload-pool="$GOOGLE_CLOUD_PROJECT.svc.id.goog"

# Bind roles/aiplatform.user to the simian-system/simian-controller KSA.
GOOGLE_CLOUD_PROJECT=my-project dev/tools/grant-vertex-access
```

The binding is project-wide and outlives the cluster: run
`dev/tools/grant-vertex-access --revoke` when you delete it. Without it the
controller still starts, but its LLM calls are refused. Set
`llm.vertex.project` and `llm.vertex.location` to match.

### 3. The controller

Each chart version's default image tag is its `appVersion`, published to
`ghcr.io/go-steer/simian-agent` when that version is tagged, so a checkout of a
release tag installs as-is:

```bash
helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace \
    --set llm.vertex.project="$GOOGLE_CLOUD_PROJECT" \
    -f examples/values-baked-defaults.yaml
```

To run something newer than the last release — `main`, or a branch — build
and push your own image, for example to Artifact Registry in the cluster's
project, which the nodes can usually pull from without extra setup:

```bash
gcloud artifacts repositories create simian --repository-format=docker --location="$REGION"
gcloud auth configure-docker "$REGION-docker.pkg.dev"
make image-push IMAGE_REGISTRY="$REGION-docker.pkg.dev" \
    IMAGE_NAME="$GOOGLE_CLOUD_PROJECT/simian/simian-agent" VERSION="$(git rev-parse --short HEAD)"
helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace \
    --set image.repository="$REGION-docker.pkg.dev/$GOOGLE_CLOUD_PROJECT/simian/simian-agent" \
    --set image.tag="$(git rev-parse --short HEAD)" \
    --set llm.vertex.project="$GOOGLE_CLOUD_PROJECT"
```

Install the chart from the same checkout as the image. The chart renders the
controller's flags, and a binary older than the chart stops on the first flag
it does not know.

### 4. Arenas, workloads, baselines

The autonomous loop only acts on a namespace that is an arena *and* has a
baseline. Until the baseline exists its health gate fails every cycle with
`no baseline cached for namespace`, and nothing is applied. The order is:

```bash
# 1. The arena: namespace, eligibility annotation, chaos RBAC. Name any
#    workload that must never be faulted — a load generator, say — so the
#    planner and the executor leave it alone.
bin/simian arena create payments \
    --annotation simian.chaos/exclude-workloads=loadgenerator

# 2. The workloads — your own manifests, or a built-in SUT.
kubectl -n payments apply -f my-app/

# 3. The baseline, captured by the controller so its health gate can read it.
kubectl -n simian-system port-forward svc/simian-controller 8081:8081 &
bin/simian baseline establish --namespace payments
```

For Online Boutique, `bin/simian sut deploy --namespace boutique --create-arena
--use-controller` does all three; add the exclusion afterwards with
`kubectl annotate ns boutique simian.chaos/exclude-workloads=loadgenerator`. Baselines are stored in a ConfigMap in the
arena and survive controller restarts. The SUT path needs the controller to
deploy into the arena: install with `sutInController.enabled=true` (the
overlay does). Then turn the loop on:

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set autonomous.enabled=true --set 'autonomous.namespaces={payments}'
```

## Agents that triage the cluster

Simian creates the chaos; other teams' agents are what diagnose it. Give them
any read role without the `chaos-mesh.org` API group, and they read the
cluster as it really is without being able to read the answer: neither
Simian's fault objects nor Chaos Mesh's per-pod `PodNetworkChaos` /
`PodHttpChaos` / `PodIOChaos` objects, which sit in the faulted namespace and
spell out the fault while it runs. The built-in `view` ClusterRole qualifies —
Chaos Mesh does not aggregate its resources into it — but it also leaves out
secrets and RBAC objects, and an agent built on k8s-lookout needs both: its
health, dependency-edge, event and blast-radius checks fail without them.
`dev/tools/eval-sre-agent` binds `view` plus secrets, RBAC and the
cluster-scoped objects lookout reads (`simian-vantage-lookout`); copy that.
Check an agent's identity before pointing it at an arena:

```bash
kubectl auth can-i list networkchaos.chaos-mesh.org -n payments \
    --as=system:serviceaccount:agents:triage-agent        # expect: no
kubectl auth can-i list podnetworkchaos.chaos-mesh.org -n payments \
    --as=system:serviceaccount:agents:triage-agent        # expect: no
```

An agent running as `cluster-admin`, or with a wildcard read role, can read
both and will report a fault by name (#129). Workload objects Simian's other
engines create — a NetworkPolicy, a synthesized Deployment — are visible to
`view` by nature; they carry only a `simian.chaos/managed` label, never the
fault's kind.

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
