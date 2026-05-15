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

package arena

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newNS(name string, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}

func TestAnnotationEligibilityTrueWhenAnnotated(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newNS("yes", map[string]string{EligibilityAnnotation: "true"}),
	)
	e := NewAnnotationEligibility(k8s)
	ok, err := e.IsEligible(ctx, "yes")
	if err != nil {
		t.Fatalf("IsEligible: %v", err)
	}
	if !ok {
		t.Error("expected eligible")
	}
}

func TestAnnotationEligibilityFalseWhenMissing(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset()
	e := NewAnnotationEligibility(k8s)
	ok, err := e.IsEligible(ctx, "missing-ns")
	if err != nil {
		t.Fatalf("IsEligible: %v", err)
	}
	if ok {
		t.Error("expected not eligible (missing namespace)")
	}
}

func TestAnnotationEligibilityFalseWhenAnnotationFalse(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newNS("opted-out", map[string]string{EligibilityAnnotation: "false"}),
	)
	e := NewAnnotationEligibility(k8s)
	ok, err := e.IsEligible(ctx, "opted-out")
	if err != nil {
		t.Fatalf("IsEligible: %v", err)
	}
	if ok {
		t.Error("expected not eligible (annotation=false)")
	}
}

func TestAnnotationEligibilityExcludedWorkloads(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newNS("with-excludes", map[string]string{
			EligibilityAnnotation:      "true",
			ExcludeWorkloadsAnnotation: "loadgenerator, redis-cart , emailservice",
		}),
	)
	e := NewAnnotationEligibility(k8s)
	got, err := e.ExcludedWorkloads(ctx, "with-excludes")
	if err != nil {
		t.Fatalf("ExcludedWorkloads: %v", err)
	}
	want := []string{"loadgenerator", "redis-cart", "emailservice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAnnotationEligibilityExcludedWorkloadsEmpty(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newNS("nada", map[string]string{EligibilityAnnotation: "true"}),
	)
	e := NewAnnotationEligibility(k8s)
	got, err := e.ExcludedWorkloads(ctx, "nada")
	if err != nil {
		t.Fatalf("ExcludedWorkloads: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no exclusions, got %v", got)
	}
}
