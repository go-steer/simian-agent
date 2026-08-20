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

package main

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/arena"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func annotatedNS(name string, eligible bool) *corev1.Namespace {
	ann := map[string]string{}
	if eligible {
		ann[arena.EligibilityAnnotation] = "true"
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}}
}

// The orphan reaper's arena resolver has to work in the mode the chart actually
// ships (`eligibleNamespaces: []` → annotation lookup). Wiring it to the empty
// --eligible-namespace slice compiles, runs, and silently reaps nothing.
func TestBuildEligibilityResolvesArenasInAnnotationMode(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		annotatedNS("boutique-1", true),
		annotatedNS("kube-system", false),
	)

	elig, arenas := buildEligibility(k8s, nil, quietLogger())

	if _, ok := elig.(*arena.AnnotationEligibility); !ok {
		t.Fatalf("with no --eligible-namespace flags, expected annotation lookup, got %T", elig)
	}
	if arenas == nil {
		t.Fatal("annotation mode produced no arena resolver; the orphan scan would never run")
	}
	got, err := arenas(ctx)
	if err != nil {
		t.Fatalf("resolve arenas: %v", err)
	}
	if want := []string{"boutique-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("arenas = %v, want %v", got, want)
	}
}

// Static mode must agree with itself: the reaper sweeps exactly the namespaces
// the executor would let a fault into.
func TestBuildEligibilityResolvesArenasInStaticMode(t *testing.T) {
	ctx := context.Background()
	flags := []string{"ns-a", "ns-b"}

	elig, arenas := buildEligibility(fake.NewClientset(), flags, quietLogger())

	got, err := arenas(ctx)
	if err != nil {
		t.Fatalf("resolve arenas: %v", err)
	}
	if want := []string{"ns-a", "ns-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arenas = %v, want %v", got, want)
	}
	for _, ns := range got {
		ok, err := elig.IsEligible(ctx, ns)
		if err != nil || !ok {
			t.Errorf("resolver named %q but IsEligible says (%v, %v)", ns, ok, err)
		}
	}

	// The resolver must not alias the caller's slice: a later mutation of the
	// flag backing array would silently widen the reaper's blast radius.
	flags[0] = "kube-system"
	if got, _ := arenas(ctx); got[0] != "ns-a" {
		t.Errorf("arena list followed a mutation of the flag slice: %v", got)
	}
}
