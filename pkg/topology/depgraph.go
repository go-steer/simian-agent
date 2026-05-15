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

package topology

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
)

// EdgesFromEnvVars infers caller→callee edges from container env vars whose
// values reference Service hostnames in the same namespace. The convention
// in microservice catalogs (Online Boutique, hipster-shop, many demo apps)
// is `FOO_SERVICE_ADDR=foo:7070` or `FOO_SERVICE_ADDR=foo.default:7070` —
// we extract the service host and check it against the namespace's Services.
//
// Returned map is "src" -> sorted unique "dst" list. Best-effort heuristic:
// false positives are possible (env vars that look like service refs but
// aren't); the LLM is told the provenance so it can weigh accordingly.
func EdgesFromEnvVars(workloads []Workload, services []Service) map[string][]string {
	known := map[string]bool{}
	for _, s := range services {
		known[s.Name] = true
	}
	edges := map[string]map[string]bool{}
	for _, w := range workloads {
		src := w.Name
		for _, c := range w.Containers {
			for _, e := range c.EnvRefs {
				dst := e.Service
				if !known[dst] || dst == src {
					continue
				}
				if edges[src] == nil {
					edges[src] = map[string]bool{}
				}
				edges[src][dst] = true
			}
		}
	}
	return flatten(edges)
}

// EdgesFromNetworkPolicies infers caller→callee edges from NetworkPolicy
// ingress rules. A NetworkPolicy on workload D that allows ingress from
// workload S maps to a logical call edge S→D (S calls into D).
//
// Egress rules are intentionally ignored: they constrain outbound traffic
// rather than declaring callees, and the caller→callee direction we want
// is more reliably read from ingress allowlists.
//
// Selection happens by matching the NetworkPolicy's PodSelector and each
// allowed peer's PodSelector against the supplied workloads' label sets.
// Workloads in the namespace are assumed to be the universe (cross-namespace
// peers are out of scope for M3).
func EdgesFromNetworkPolicies(workloads []Workload, policies []netv1.NetworkPolicy) map[string][]string {
	edges := map[string]map[string]bool{}
	for _, np := range policies {
		dsts := matchWorkloads(workloads, np.Spec.PodSelector.MatchLabels)
		for _, ing := range np.Spec.Ingress {
			for _, peer := range ing.From {
				if peer.PodSelector == nil {
					continue
				}
				srcs := matchWorkloads(workloads, peer.PodSelector.MatchLabels)
				for _, src := range srcs {
					for _, dst := range dsts {
						if src == dst {
							continue
						}
						if edges[src] == nil {
							edges[src] = map[string]bool{}
						}
						edges[src][dst] = true
					}
				}
			}
		}
	}
	return flatten(edges)
}

// MergeEdges combines two edge maps; provenance map records which heuristic
// produced each "src->dst" key.
func MergeEdges(graph, provenance map[string][]string, add map[string][]string, label string) {
	for src, dsts := range add {
		seen := map[string]bool{}
		for _, d := range graph[src] {
			seen[d] = true
		}
		for _, d := range dsts {
			if !seen[d] {
				graph[src] = append(graph[src], d)
				seen[d] = true
			}
			key := src + "->" + d
			provenance[key] = appendUnique(provenance[key], label)
		}
		sort.Strings(graph[src])
	}
}

// matchWorkloads returns the names of workloads whose labels are a superset
// of the supplied selector. An empty selector matches no workloads (rather
// than all) — empty-selector NetworkPolicy semantics are explicitly out of
// scope for M3 because they're rarely useful for dep-graph inference.
func matchWorkloads(workloads []Workload, selector map[string]string) []string {
	if len(selector) == 0 {
		return nil
	}
	var out []string
	for _, w := range workloads {
		if labelsSupersetOf(w.Labels, selector) {
			out = append(out, w.Name)
		}
	}
	sort.Strings(out)
	return out
}

func labelsSupersetOf(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func flatten(m map[string]map[string]bool) map[string][]string {
	out := map[string][]string{}
	for src, dsts := range m {
		list := make([]string, 0, len(dsts))
		for d := range dsts {
			list = append(list, d)
		}
		sort.Strings(list)
		out[src] = list
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// envRefsFromContainer extracts EnvServiceRef entries from a container
// spec. Used by the discoverer when building Workload summaries; pure so
// it's testable without informers.
func envRefsFromContainer(c corev1.Container) []EnvServiceRef {
	var out []EnvServiceRef
	for _, e := range c.Env {
		if e.Value == "" {
			continue
		}
		host, port, ok := splitHostPort(e.Value)
		if !ok {
			continue
		}
		// Strip ".namespace" suffix if present.
		if i := strings.IndexByte(host, '.'); i > 0 {
			host = host[:i]
		}
		out = append(out, EnvServiceRef{EnvName: e.Name, Service: host, Port: port})
	}
	return out
}

// splitHostPort parses "host:port" loosely. Returns (host, port, ok).
// Rejects values without a colon, values that look like URLs, and values
// where either side contains characters incompatible with DNS labels.
func splitHostPort(v string) (string, string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", false
	}
	if strings.Contains(v, "://") || strings.Contains(v, "/") {
		return "", "", false
	}
	i := strings.LastIndexByte(v, ':')
	if i <= 0 || i == len(v)-1 {
		return "", "", false
	}
	host := v[:i]
	port := v[i+1:]
	if !validDNSLabelish(host) || !validPortDigits(port) {
		return "", "", false
	}
	return host, port, true
}

func validDNSLabelish(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

func validPortDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
