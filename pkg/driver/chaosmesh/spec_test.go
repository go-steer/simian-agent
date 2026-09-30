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
	"strings"
	"testing"
	"time"

	clienttesting "k8s.io/client-go/testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// chaos-dns-server takes "*" only as the last character of a pattern. Every
// other placement passes the CRD schema, is created, and then fails per pod
// with "pattern ... not valid" — found on the 2026-09-30 trial as three
// DNSChaos faults that sat un-injected for their whole confirmation wait
// (#160).
func TestADNSPatternWithAWildcardAnywhereButTheEndIsRefused(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		ok      bool
	}{
		{"productcatalogservice.boutique.svc.cluster.local", true},
		{"productcatalogservice.*", true},
		{"chaos-mes?.org", true},
		{"*.bank.svc.cluster.local", false},
		{"*emailservice.boutique.svc.cluster.local", false},
		{"*.productcatalogservice.svc.cluster.local", false},
		{"chaos-*.org", false},
	} {
		err := checkDNSPatterns(map[string]any{"patterns": []any{tc.pattern}})
		if (err == nil) != tc.ok {
			t.Errorf("pattern %q: err = %v, want ok=%v", tc.pattern, err, tc.ok)
		}
	}
}

// Refused at apply, before anything is created, so the failure is immediate
// and names the pattern rather than surfacing thirty seconds later as a fault
// that did not inject.
func TestApplyRefusesABadDNSPatternWithoutCreatingAnything(t *testing.T) {
	d, fake := newTestDriver(t)

	_, err := d.Apply(context.Background(), simian.FaultManifest{
		UID:          "u-1",
		Engine:       simian.EngineChaosMesh,
		APIVersion:   APIGroup + "/v1alpha1",
		ResourceKind: "DNSChaos",
		Duration:     time.Minute,
		Targets:      []simian.TargetRef{{Namespace: "bank"}},
		Spec: map[string]any{
			"action":   "error",
			"mode":     "all",
			"patterns": []any{"*.bank.svc.cluster.local"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "*.bank.svc.cluster.local") {
		t.Fatalf("apply err = %v, want a refusal naming the pattern", err)
	}
	for _, a := range fake.Actions() {
		if _, ok := a.(clienttesting.CreateActionImpl); ok {
			t.Errorf("a create was issued for a spec that was refused")
		}
	}
}
