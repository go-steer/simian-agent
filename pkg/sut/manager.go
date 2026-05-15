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

package sut

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

// FieldManager is the server-side-apply field manager string used for all
// SUT-applied resources. Stable so re-applies update without conflict.
const FieldManager = "simian-sut"

// Manager owns SUT lifecycle: apply manifests, wait for steady state, capture
// baseline, tear down. The arena (namespace + chaos-SA RoleBinding) must
// already exist; create it via pkg/arena before calling Deploy, or pass
// CreateArena=true on DeployOptions to compose the two.
type Manager struct {
	K8s    kubernetes.Interface
	Dyn    dynamic.Interface
	Disco  discovery.CachedDiscoveryInterface
	mapper *restmapper.DeferredDiscoveryRESTMapper

	Registry Registry

	mu        sync.RWMutex
	baselines map[string]Baseline // keyed by namespace
}

// NewManager constructs a Manager.
func NewManager(k8s kubernetes.Interface, dyn dynamic.Interface, disco discovery.CachedDiscoveryInterface, registry Registry) *Manager {
	if registry == nil {
		registry = Default
	}
	return &Manager{
		K8s:       k8s,
		Dyn:       dyn,
		Disco:     disco,
		mapper:    restmapper.NewDeferredDiscoveryRESTMapper(disco),
		Registry:  registry,
		baselines: map[string]Baseline{},
	}
}

// DeployOptions configures Deploy.
type DeployOptions struct {
	Namespace string
	SUTName   string
}

// Deploy applies a SUT into the given namespace, waits for the declared
// workloads to reach Ready, holds for the stability window, and returns the
// captured baseline. Subsequent calls with the same args re-apply (idempotent
// via server-side apply) and re-capture the baseline.
func (m *Manager) Deploy(ctx context.Context, opts DeployOptions) (*Baseline, error) {
	if opts.Namespace == "" {
		return nil, fmt.Errorf("sut: namespace is required")
	}
	s, ok := m.Registry.Get(opts.SUTName)
	if !ok {
		return nil, fmt.Errorf("sut: %q is not registered", opts.SUTName)
	}

	docs, err := splitYAML(s.Manifests())
	if err != nil {
		return nil, fmt.Errorf("sut: parse manifests: %w", err)
	}
	for _, doc := range docs {
		doc.SetNamespace(opts.Namespace)
		if err := m.applyOne(ctx, doc); err != nil {
			return nil, fmt.Errorf("sut: apply %s/%s: %w", doc.GetKind(), doc.GetName(), err)
		}
	}

	cfg := s.BaselineConfig()
	bl, err := m.waitForBaseline(ctx, opts.Namespace, s, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.baselines[opts.Namespace] = *bl
	m.mu.Unlock()
	return bl, nil
}

// DestroyOptions configures Destroy.
type DestroyOptions struct {
	Namespace string
	SUTName   string
}

// Destroy removes the SUT's resources from the namespace. Does not delete the
// namespace itself; the arena layer owns that lifecycle.
func (m *Manager) Destroy(ctx context.Context, opts DestroyOptions) error {
	if opts.Namespace == "" {
		return fmt.Errorf("sut: namespace is required")
	}
	s, ok := m.Registry.Get(opts.SUTName)
	if !ok {
		return fmt.Errorf("sut: %q is not registered", opts.SUTName)
	}
	docs, err := splitYAML(s.Manifests())
	if err != nil {
		return fmt.Errorf("sut: parse manifests: %w", err)
	}
	// Delete in reverse order — Services/SAs after Deployments — so dependent
	// resources don't briefly reference missing parents.
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		doc.SetNamespace(opts.Namespace)
		if err := m.deleteOne(ctx, doc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("sut: delete %s/%s: %w", doc.GetKind(), doc.GetName(), err)
		}
	}
	m.mu.Lock()
	delete(m.baselines, opts.Namespace)
	m.mu.Unlock()
	return nil
}

// Baseline returns the cached baseline for a namespace, if any.
func (m *Manager) Baseline(namespace string) (Baseline, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	bl, ok := m.baselines[namespace]
	return bl, ok
}

func (m *Manager) applyOne(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := m.gvrFor(obj.GroupVersionKind())
	if err != nil {
		return err
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	force := true
	_, err = m.Dyn.Resource(gvr).Namespace(obj.GetNamespace()).Patch(
		ctx, obj.GetName(), types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force},
	)
	return err
}

func (m *Manager) deleteOne(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := m.gvrFor(obj.GroupVersionKind())
	if err != nil {
		return err
	}
	return m.Dyn.Resource(gvr).Namespace(obj.GetNamespace()).Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
}

func (m *Manager) gvrFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	mapping, err := m.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("REST mapping for %s: %w", gvk, err)
	}
	return mapping.Resource, nil
}

// waitForBaseline polls workload status until all expected workloads are
// Ready, then holds the stability window before declaring baseline.
func (m *Manager) waitForBaseline(ctx context.Context, namespace string, s SUT, cfg BaselineConfig) (*Baseline, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 5 * time.Minute
	}
	deadline := time.Now().Add(cfg.ReadyTimeout)
	expected := s.ExpectedWorkloads()

	var firstReadyAt time.Time
	for {
		statuses, allReady, err := m.workloadStatuses(ctx, namespace, expected)
		if err != nil {
			return nil, fmt.Errorf("sut: workload status: %w", err)
		}
		if allReady {
			if firstReadyAt.IsZero() {
				firstReadyAt = time.Now()
			}
			if time.Since(firstReadyAt) >= cfg.StabilityWindow {
				return &Baseline{
					Namespace:       namespace,
					SUT:             s.Name(),
					EstablishedAt:   time.Now().UTC(),
					StabilityWindow: cfg.StabilityWindow,
					Workloads:       statuses,
				}, nil
			}
		} else {
			// Workloads regressed during stability window — restart timing.
			firstReadyAt = time.Time{}
		}
		if time.Now().After(deadline) {
			return nil, &BaselineTimeoutError{
				Namespace: namespace,
				SUT:       s.Name(),
				Statuses:  statuses,
				Elapsed:   cfg.ReadyTimeout,
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// workloadStatuses returns one status per expected workload, sorted by
// (Kind,Name) for stable output, plus a summary bool.
func (m *Manager) workloadStatuses(ctx context.Context, namespace string, expected []WorkloadRef) ([]WorkloadStatus, bool, error) {
	out := make([]WorkloadStatus, 0, len(expected))
	allReady := len(expected) > 0
	for _, w := range expected {
		st, err := m.workloadStatus(ctx, namespace, w)
		if err != nil {
			if apierrors.IsNotFound(err) {
				out = append(out, WorkloadStatus{Kind: w.Kind, Name: w.Name})
				allReady = false
				continue
			}
			return nil, false, err
		}
		out = append(out, st)
		if st.DesiredReplicas == 0 || st.ReadyReplicas < st.DesiredReplicas {
			allReady = false
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, allReady, nil
}

func (m *Manager) workloadStatus(ctx context.Context, namespace string, w WorkloadRef) (WorkloadStatus, error) {
	switch w.Kind {
	case "Deployment", "":
		dep, err := m.K8s.AppsV1().Deployments(namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatus{Kind: "Deployment", Name: w.Name}, err
		}
		return WorkloadStatus{
			Kind:            "Deployment",
			Name:            w.Name,
			DesiredReplicas: deploymentDesired(dep),
			ReadyReplicas:   dep.Status.ReadyReplicas,
		}, nil
	case "StatefulSet":
		ss, err := m.K8s.AppsV1().StatefulSets(namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatus{Kind: "StatefulSet", Name: w.Name}, err
		}
		return WorkloadStatus{
			Kind:            "StatefulSet",
			Name:            w.Name,
			DesiredReplicas: statefulSetDesired(ss),
			ReadyReplicas:   ss.Status.ReadyReplicas,
		}, nil
	default:
		return WorkloadStatus{Kind: w.Kind, Name: w.Name}, fmt.Errorf("unsupported workload kind %q", w.Kind)
	}
}

func deploymentDesired(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func statefulSetDesired(s *appsv1.StatefulSet) int32 {
	if s.Spec.Replicas == nil {
		return 1
	}
	return *s.Spec.Replicas
}

// splitYAML parses a multi-document YAML stream into unstructured objects.
// Empty documents (separator-only) are skipped.
func splitYAML(data []byte) ([]*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: raw})
	}
	return out, nil
}

// BaselineTimeoutError is returned when waitForBaseline gives up. Carries the
// last-seen workload statuses so the caller (CLI/MCP) can surface what was
// missing.
type BaselineTimeoutError struct {
	Namespace string
	SUT       string
	Statuses  []WorkloadStatus
	Elapsed   time.Duration
}

func (e *BaselineTimeoutError) Error() string {
	missing := []string{}
	for _, s := range e.Statuses {
		if s.ReadyReplicas < s.DesiredReplicas {
			missing = append(missing, fmt.Sprintf("%s/%s (%d/%d)", s.Kind, s.Name, s.ReadyReplicas, s.DesiredReplicas))
		}
	}
	return fmt.Sprintf("sut: baseline not reached for %s in namespace %q within %s; not-ready: %v",
		e.SUT, e.Namespace, e.Elapsed, missing)
}
