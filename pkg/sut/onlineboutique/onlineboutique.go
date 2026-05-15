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

// Package onlineboutique embeds Google Cloud's Online Boutique microservices
// demo (https://github.com/GoogleCloudPlatform/microservices-demo) as a
// Simian SUT. The embedded manifests come from upstream release v0.10.2 and
// are licensed under Apache 2.0; see manifests/LICENSE.
package onlineboutique

import (
	_ "embed"
	"time"

	"github.com/go-steer/simian-agent/pkg/sut"
)

const Name = "online-boutique"

//go:embed manifests/kubernetes-manifests.yaml
var manifestsYAML []byte

type onlineBoutique struct{}

func (o *onlineBoutique) Name() string { return Name }

func (o *onlineBoutique) Description() string {
	return "Google Cloud Online Boutique microservices demo (12 services + load generator)"
}

func (o *onlineBoutique) Manifests() []byte {
	// Return a copy so callers can't mutate our embedded buffer.
	out := make([]byte, len(manifestsYAML))
	copy(out, manifestsYAML)
	return out
}

func (o *onlineBoutique) ExpectedWorkloads() []sut.WorkloadRef {
	return []sut.WorkloadRef{
		{Kind: "Deployment", Name: "frontend"},
		{Kind: "Deployment", Name: "cartservice"},
		{Kind: "Deployment", Name: "productcatalogservice"},
		{Kind: "Deployment", Name: "currencyservice"},
		{Kind: "Deployment", Name: "paymentservice"},
		{Kind: "Deployment", Name: "shippingservice"},
		{Kind: "Deployment", Name: "emailservice"},
		{Kind: "Deployment", Name: "checkoutservice"},
		{Kind: "Deployment", Name: "recommendationservice"},
		{Kind: "Deployment", Name: "adservice"},
		{Kind: "Deployment", Name: "redis-cart"},
		{Kind: "Deployment", Name: "loadgenerator"},
	}
}

func (o *onlineBoutique) BaselineConfig() sut.BaselineConfig {
	cfg := sut.DefaultBaselineConfig()
	// Online Boutique can take a couple minutes to come up cold on a small
	// node pool — bump the ready timeout above the default.
	cfg.ReadyTimeout = 8 * time.Minute
	return cfg
}

// Register adds the Online Boutique SUT to the package-level registry.
// Imported for side effect from the binary's main package.
func Register() { sut.Default.MustRegister(&onlineBoutique{}) }
