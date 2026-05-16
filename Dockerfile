# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simian ./cmd/simian
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simian-envoy-agent ./cmd/simian-envoy-agent

# The image carries two binaries: the simian controller (ENTRYPOINT)
# and the simian-envoy-agent probe-rewriter sidecar. Injected
# Deployments override Command to /usr/local/bin/simian-envoy-agent;
# the chart's controller install uses the default ENTRYPOINT.
FROM gcr.io/distroless/static
COPY --from=build /out/simian /usr/local/bin/simian
COPY --from=build /out/simian-envoy-agent /usr/local/bin/simian-envoy-agent
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/simian"]
CMD ["serve"]
