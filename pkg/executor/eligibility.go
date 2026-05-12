package executor

import (
	"context"
	"strings"
)

// EligibilityAnnotation is the namespace-level opt-in annotation.
const EligibilityAnnotation = "simian.chaos/eligible"

// ExcludeWorkloadsAnnotation is the optional fine-grained workload exclusion
// list, comma-separated.
const ExcludeWorkloadsAnnotation = "simian.chaos/exclude-workloads"

// EligibilityChecker abstracts the namespace-annotation lookup so the executor
// can be unit-tested without a real Kubernetes client.
type EligibilityChecker interface {
	// IsEligible returns true if the namespace carries the eligibility
	// annotation set to "true".
	IsEligible(ctx context.Context, namespace string) (bool, error)

	// ExcludedWorkloads returns the parsed exclusion list for the namespace.
	ExcludedWorkloads(ctx context.Context, namespace string) ([]string, error)
}

// StaticEligibility is an EligibilityChecker backed by a fixed map. Useful for
// tests and for installations that want to bypass annotation lookup entirely.
type StaticEligibility struct {
	Eligible       map[string]bool
	Exclusions     map[string][]string
}

// IsEligible implements EligibilityChecker.
func (s *StaticEligibility) IsEligible(_ context.Context, namespace string) (bool, error) {
	return s.Eligible[namespace], nil
}

// ExcludedWorkloads implements EligibilityChecker.
func (s *StaticEligibility) ExcludedWorkloads(_ context.Context, namespace string) ([]string, error) {
	return s.Exclusions[namespace], nil
}

// ParseExcludeAnnotation parses a comma-separated workload exclusion string.
func ParseExcludeAnnotation(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
