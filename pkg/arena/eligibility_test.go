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
