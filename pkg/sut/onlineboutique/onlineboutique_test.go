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

package onlineboutique

import (
	"testing"

	"github.com/go-steer/simian-agent/pkg/sut"
)

func TestImplementsOptionalProviders(t *testing.T) {
	var s sut.SUT = &onlineBoutique{}

	fp, ok := s.(sut.EnvoyFaultPortsProvider)
	if !ok {
		t.Fatal("onlineBoutique should implement EnvoyFaultPortsProvider")
	}
	for _, port := range fp.EnvoyFaultPorts() {
		if port == 6379 {
			t.Error("EnvoyFaultPorts should NOT include 6379: redis-cart is raw TCP and is skipped via NoEnvoyInjectionWorkloads")
		}
	}

	np, ok := s.(sut.NoEnvoyInjectionWorkloadsProvider)
	if !ok {
		t.Fatal("onlineBoutique should implement NoEnvoyInjectionWorkloadsProvider")
	}
	names := np.NoEnvoyInjectionWorkloads()
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, must := range []string{"loadgenerator", "redis-cart"} {
		if !got[must] {
			t.Errorf("NoEnvoyInjectionWorkloads should include %q; got %v", must, names)
		}
	}
}
