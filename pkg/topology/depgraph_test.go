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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEdgesFromEnvVars(t *testing.T) {
	workloads := []Workload{
		{
			Name:   "frontend",
			Labels: map[string]string{"app": "frontend"},
			Containers: []ContainerSummary{{
				Name: "server",
				EnvRefs: []EnvServiceRef{
					{EnvName: "CART_SERVICE_ADDR", Service: "cartservice", Port: "7070"},
					{EnvName: "PRODUCT_CATALOG_SERVICE_ADDR", Service: "productcatalogservice", Port: "3550"},
					{EnvName: "DOES_NOT_EXIST_ADDR", Service: "ghostservice", Port: "1234"},
					// self-reference must be dropped
					{EnvName: "SELF_ADDR", Service: "frontend", Port: "8080"},
				},
			}},
		},
	}
	services := []Service{
		{Name: "frontend"},
		{Name: "cartservice"},
		{Name: "productcatalogservice"},
	}
	got := EdgesFromEnvVars(workloads, services)
	want := map[string][]string{
		"frontend": {"cartservice", "productcatalogservice"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EdgesFromEnvVars = %v, want %v", got, want)
	}
}

func TestEdgesFromNetworkPolicies(t *testing.T) {
	workloads := []Workload{
		{Name: "frontend", Labels: map[string]string{"app": "frontend"}},
		{Name: "cartservice", Labels: map[string]string{"app": "cartservice"}},
	}
	policies := []netv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-frontend-to-cart"},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "cartservice"}},
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From: []netv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
				}},
			}},
		},
	}}
	got := EdgesFromNetworkPolicies(workloads, policies)
	want := map[string][]string{"frontend": {"cartservice"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EdgesFromNetworkPolicies = %v, want %v", got, want)
	}
}

func TestEdgesFromNetworkPolicies_EmptySelectorMatchesNothing(t *testing.T) {
	workloads := []Workload{
		{Name: "a", Labels: map[string]string{"app": "a"}},
		{Name: "b", Labels: map[string]string{"app": "b"}},
	}
	policies := []netv1.NetworkPolicy{{
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
			Ingress: []netv1.NetworkPolicyIngressRule{{
				From: []netv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{}, // empty
				}},
			}},
		},
	}}
	got := EdgesFromNetworkPolicies(workloads, policies)
	if len(got) != 0 {
		t.Fatalf("empty selector should produce no edges, got %v", got)
	}
}

func TestMergeEdges_RecordsProvenance(t *testing.T) {
	graph := map[string][]string{}
	prov := map[string][]string{}
	MergeEdges(graph, prov, map[string][]string{"a": {"b"}}, "networkpolicy")
	MergeEdges(graph, prov, map[string][]string{"a": {"b", "c"}}, "envvar")
	if !reflect.DeepEqual(graph["a"], []string{"b", "c"}) {
		t.Errorf("graph[a] = %v, want [b c]", graph["a"])
	}
	if got := prov["a->b"]; !reflect.DeepEqual(got, []string{"networkpolicy", "envvar"}) {
		t.Errorf("prov[a->b] = %v, want [networkpolicy envvar]", got)
	}
	if got := prov["a->c"]; !reflect.DeepEqual(got, []string{"envvar"}) {
		t.Errorf("prov[a->c] = %v, want [envvar]", got)
	}
}

func TestEnvRefsFromContainer(t *testing.T) {
	c := corev1.Container{
		Env: []corev1.EnvVar{
			{Name: "CART_SERVICE_ADDR", Value: "cartservice:7070"},
			{Name: "PRODUCT_ADDR", Value: "productcatalogservice.default:3550"},
			{Name: "AD_URL", Value: "http://adservice/recommend"}, // URL → ignored
			{Name: "REGION", Value: "us-central1"},                // no port → ignored
			{Name: "PORT", Value: "8080"},                         // no host → ignored
		},
	}
	got := envRefsFromContainer(c)
	want := []EnvServiceRef{
		{EnvName: "CART_SERVICE_ADDR", Service: "cartservice", Port: "7070"},
		{EnvName: "PRODUCT_ADDR", Service: "productcatalogservice", Port: "3550"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envRefsFromContainer = %v, want %v", got, want)
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port string
		ok   bool
	}{
		{"cartservice:7070", "cartservice", "7070", true},
		{"foo.bar.baz:80", "foo.bar.baz", "80", true},
		{"http://foo:80", "", "", false},
		{"foo/bar:80", "", "", false},
		{":80", "", "", false},
		{"foo:", "", "", false},
		{"foo", "", "", false},
		{"", "", "", false},
		{"foo:abc", "", "", false},
	}
	for _, c := range cases {
		host, port, ok := splitHostPort(c.in)
		if host != c.host || port != c.port || ok != c.ok {
			t.Errorf("splitHostPort(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, host, port, ok, c.host, c.port, c.ok)
		}
	}
}
