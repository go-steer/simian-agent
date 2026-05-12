package arena

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AnnotationEligibility implements executor.EligibilityChecker by reading
// namespace annotations directly from the cluster. The executor consults this
// once per fault application; M1's StaticEligibility (set via
// `--eligible-namespace` flags) remains supported for installations that want
// to bypass annotation lookup entirely.
//
// This type lives in pkg/arena rather than pkg/executor to avoid an executor →
// kubernetes import edge: executor is engine-agnostic; arena owns all the
// annotation-and-RBAC concerns.
type AnnotationEligibility struct {
	K8s kubernetes.Interface
}

// NewAnnotationEligibility constructs an AnnotationEligibility.
func NewAnnotationEligibility(k8s kubernetes.Interface) *AnnotationEligibility {
	return &AnnotationEligibility{K8s: k8s}
}

// IsEligible returns true when the namespace exists and carries
// `simian.chaos/eligible: "true"`. Missing namespaces return (false, nil) so
// the executor's safety stage produces a clean rejection rather than a
// not-found error.
func (e *AnnotationEligibility) IsEligible(ctx context.Context, namespace string) (bool, error) {
	if namespace == "" {
		return false, nil
	}
	ns, err := e.K8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("eligibility: get namespace %q: %w", namespace, err)
	}
	return ns.Annotations[EligibilityAnnotation] == "true", nil
}

// ExcludedWorkloads parses the comma-separated exclusion annotation.
func (e *AnnotationEligibility) ExcludedWorkloads(ctx context.Context, namespace string) ([]string, error) {
	ns, err := e.K8s.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eligibility: get namespace %q: %w", namespace, err)
	}
	v := ns.Annotations[ExcludeWorkloadsAnnotation]
	return parseExcludeAnnotation(v), nil
}

func parseExcludeAnnotation(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			s := v[start:i]
			for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
				s = s[1:]
			}
			for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
				s = s[:len(s)-1]
			}
			if s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	return out
}
