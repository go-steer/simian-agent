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
	"fmt"
	"strings"
)

// checkSpec refuses what the API server accepts but Chaos Mesh cannot run.
// Strict field validation already stops a spec with a field its kind does not
// have; this is for values that pass the CRD schema and fail further in, where
// the cost is a fault that sits un-injected until the confirmation wait gives
// up instead of an error at apply.
func checkSpec(kind string, spec map[string]any) error {
	switch kind {
	case "DNSChaos":
		return checkDNSPatterns(spec)
	}
	return nil
}

// checkDNSPatterns enforces the rule the CRD only states in prose: a "*" in a
// DNSChaos pattern must be its last character. chaos-dns-server rejects any
// other placement ("pattern *.bank.svc.cluster.local not valid") after the
// object is created, per pod, so the fault never lands.
func checkDNSPatterns(spec map[string]any) error {
	var patterns []string
	switch v := spec["patterns"].(type) {
	case []string:
		patterns = v
	case []any:
		for _, p := range v {
			if s, ok := p.(string); ok {
				patterns = append(patterns, s)
			}
		}
	}
	for _, p := range patterns {
		if i := strings.Index(p, "*"); i >= 0 && i != len(p)-1 {
			return fmt.Errorf("DNSChaos pattern %q: Chaos Mesh only accepts \"*\" as the last character; name the service in full, e.g. <service>.<namespace>.svc.cluster.local", p)
		}
	}
	return nil
}
