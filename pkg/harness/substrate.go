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

package harness

import (
	"context"
	"fmt"

	"github.com/go-steer/simian-agent/pkg/sut"
)

// SUTSubstrates is the Substrates implementation backed by the SUT registry
// the product already ships — the same manifests, the same readiness wait and
// the same stability hold that `simian sut deploy` uses.
//
// The same one deliberately. A scenario whose substrate came up under
// harness-only code would be measuring a deployment path no operator runs, and
// the first thing that stops catching is the SUT's own readiness gate being
// wrong. "Landed is not the same as steady" applies to the thing the fault is
// aimed at, not only to the fault.
type SUTSubstrates struct {
	Manager  *sut.Manager
	Registry sut.Registry
}

// Known reports whether the registry has this SUT.
func (s *SUTSubstrates) Known(name string) bool {
	if s == nil || s.Registry == nil {
		return false
	}
	_, ok := s.Registry.Get(name)
	return ok
}

// Deploy applies the SUT and returns once its declared workloads have been
// Ready for the SUT's own stability window. The baseline it captures on the
// way is discarded: the harness grades against a scenario's expectations, and
// a second opinion about what healthy looks like would be a second gate with
// nobody reading its answer.
//
// Envoy injection is off. It is opt-in in `simian sut deploy` too, for the
// reason recorded there — the iptables interception breaks gRPC kubelet probes
// — and a substrate whose pods are NotReady for a reason the scenario did not
// inject is a scenario that grades the sidecar.
func (s *SUTSubstrates) Deploy(ctx context.Context, namespace, name string) error {
	if s == nil || s.Manager == nil {
		return fmt.Errorf("harness: no SUT manager configured for substrate %q", name)
	}
	if _, err := s.Manager.Deploy(ctx, sut.DeployOptions{Namespace: namespace, SUTName: name}); err != nil {
		return err
	}
	return nil
}

// Destroy removes the SUT's resources. The namespace is the arena's business
// and is left alone.
func (s *SUTSubstrates) Destroy(ctx context.Context, namespace, name string) error {
	if s == nil || s.Manager == nil {
		return fmt.Errorf("harness: no SUT manager configured for substrate %q", name)
	}
	return s.Manager.Destroy(ctx, sut.DestroyOptions{Namespace: namespace, SUTName: name})
}
