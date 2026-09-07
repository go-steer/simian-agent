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

package chaosmesh

import (
	"context"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"
)

var httpChaosGVR = schema.GroupVersionResource{
	Group: APIGroup, Version: "v1alpha1", Resource: "httpchaos",
}

// cachedDiscovery adapts the fake discovery client to the cached interface the
// deferred REST mapper wants. Nothing is actually cached; Fresh/Invalidate are
// the no-ops a one-shot test needs.
type cachedDiscovery struct{ discovery.DiscoveryInterface }

func (cachedDiscovery) Fresh() bool { return true }
func (cachedDiscovery) Invalidate() {}

func newTestDriver(t *testing.T) (*Driver, *clienttesting.Fake) {
	t.Helper()

	disco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{
		Resources: []*metav1.APIResourceList{{
			GroupVersion: APIGroup + "/v1alpha1",
			APIResources: []metav1.APIResource{{
				Name: "httpchaos", SingularName: "httpchaos", Namespaced: true, Kind: "HTTPChaos",
			}},
		}},
	}}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{httpChaosGVR: "HTTPChaosList"},
	)

	d := &Driver{
		dyn:   dyn,
		disco: disco,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(
			cachedDiscovery{disco},
		),
		namePrefix: "simian-",
	}
	return d, &dyn.Fake
}

// A chaos spec is hand-written YAML, and the API server's default is to drop
// what it does not recognise and carry on. An HTTPChaos carrying `action:
// replace` — a field HTTPChaos does not have, though NetworkChaos and DNSChaos
// both do — applied cleanly, warned once on a line nobody was reading, and
// injected whatever survived. It happened to work, because the `replace` block
// is what actually drives that kind, so the fault was right by luck for as
// long as it took a live run to print the warning.
//
// That is this project's own bug class turned on the rig: applied is not the
// same as accepted. The only defence that scales to every kind Chaos Mesh
// ships — without a copy of its schemas in this repo going stale — is to make
// the API server do the checking and refuse the create.
func TestAnUnknownFieldInAChaosSpecIsRefusedRatherThanDropped(t *testing.T) {
	d, fake := newTestDriver(t)

	_, err := d.Apply(context.Background(), simian.FaultManifest{
		UID:          "u-1",
		Engine:       simian.EngineChaosMesh,
		APIVersion:   APIGroup + "/v1alpha1",
		ResourceKind: "HTTPChaos",
		Duration:     time.Minute,
		Targets:      []simian.TargetRef{{Namespace: "flow-checkout"}},
		Spec:         map[string]any{"target": "Response"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The fake client does not validate, so what is asserted is that the
	// request asks the API server to. Nothing else in Apply can produce the
	// rejection, and without the option a bad field is a warning on stderr.
	var got []metav1.CreateOptions
	for _, a := range fake.Actions() {
		if c, ok := a.(clienttesting.CreateActionImpl); ok {
			got = append(got, c.CreateOptions)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d creates, want 1", len(got))
	}
	if got[0].FieldValidation != metav1.FieldValidationStrict {
		t.Errorf("created with FieldValidation %q, want %q; an unknown field would be silently dropped and the subject graded against a fault nobody wrote",
			got[0].FieldValidation, metav1.FieldValidationStrict)
	}
}
