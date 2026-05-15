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
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Constants describing the on-disk shape of the persisted ConfigMap. The
// ConfigMap lives in the SUT's own namespace so namespace deletion is an
// automatic cleanup hook — no orphaned baselines in `simian-system` after
// `kubectl delete ns boutique-m3`.
const (
	// configMapName is the constant ConfigMap name used for every namespace's
	// persisted baseline. One ConfigMap per namespace; namespace is the key.
	configMapName = "simian-baseline"

	// configMapDataKey is the data field holding the JSON-encoded Baseline.
	configMapDataKey = "baseline.json"

	// labelManagedBy and labelComponent are added so kubectl / Helm see the
	// ConfigMap as ours and so the ConfigMapStore can list+filter cluster-wide.
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "simian.go-steer.dev/persistence"

	managedByValue = "simian-agent"
	componentValue = "baseline"
)

// ConfigMapStore is a Kubernetes-native BaselineStore backed by ConfigMaps in
// the SUT's own namespace.
//
// One ConfigMap per namespace at <ns>/simian-baseline holds the JSON-encoded
// Baseline. Co-locating in the SUT namespace (rather than `simian-system`) has
// two desirable properties:
//
//  1. Namespace deletion auto-cleans the persisted baseline; no separate
//     teardown step required when an arena is destroyed by hand.
//  2. RBAC scope stays tight — the controller only needs configmaps verbs on
//     namespaces it already manages SUTs in.
type ConfigMapStore struct {
	client kubernetes.Interface
}

// NewConfigMapStore constructs a ConfigMapStore over the given clientset.
// Pass the same clientset already used for SUT manifest application — the
// store does not need cluster-admin, just configmaps verbs on SUT namespaces.
func NewConfigMapStore(client kubernetes.Interface) *ConfigMapStore {
	return &ConfigMapStore{client: client}
}

// Save writes (or upserts) the ConfigMap holding bl. Uses Update-then-Create
// to avoid losing data on conflicting updates from concurrent serves; we
// expect a single controller per namespace in practice, so contention is rare.
func (s *ConfigMapStore) Save(ctx context.Context, bl Baseline) error {
	if bl.Namespace == "" {
		return fmt.Errorf("ConfigMapStore.Save: baseline.Namespace is empty")
	}
	data, err := json.Marshal(bl)
	if err != nil {
		return fmt.Errorf("ConfigMapStore.Save: marshal: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: bl.Namespace,
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelComponent: componentValue,
			},
		},
		Data: map[string]string{configMapDataKey: string(data)},
	}
	cms := s.client.CoreV1().ConfigMaps(bl.Namespace)
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{FieldManager: FieldManager})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, cm, metav1.CreateOptions{FieldManager: FieldManager})
	}
	if err != nil {
		return fmt.Errorf("ConfigMapStore.Save %s/%s: %w", bl.Namespace, configMapName, err)
	}
	return nil
}

// Load returns the persisted baseline for a namespace, if any. A missing
// ConfigMap (NotFound) returns (zero, false, nil) — not an error.
func (s *ConfigMapStore) Load(ctx context.Context, namespace string) (Baseline, bool, error) {
	cm, err := s.client.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Baseline{}, false, nil
	}
	if err != nil {
		return Baseline{}, false, fmt.Errorf("ConfigMapStore.Load %s/%s: %w", namespace, configMapName, err)
	}
	raw, ok := cm.Data[configMapDataKey]
	if !ok || raw == "" {
		return Baseline{}, false, fmt.Errorf("ConfigMapStore.Load %s/%s: missing %q data key", namespace, configMapName, configMapDataKey)
	}
	var bl Baseline
	if err := json.Unmarshal([]byte(raw), &bl); err != nil {
		return Baseline{}, false, fmt.Errorf("ConfigMapStore.Load %s/%s: decode: %w", namespace, configMapName, err)
	}
	return bl, true, nil
}

// Delete removes the persisted baseline for a namespace. NotFound is not an
// error — Destroy may be called against a namespace that never had a baseline.
func (s *ConfigMapStore) Delete(ctx context.Context, namespace string) error {
	err := s.client.CoreV1().ConfigMaps(namespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("ConfigMapStore.Delete %s/%s: %w", namespace, configMapName, err)
}

// List returns every persisted baseline across all namespaces. Implemented as
// a cluster-wide list filtered by our managed-by label so the controller
// doesn't need to enumerate namespaces first.
func (s *ConfigMapStore) List(ctx context.Context) ([]Baseline, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", labelManagedBy, managedByValue, labelComponent, componentValue)
	cms, err := s.client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("ConfigMapStore.List: %w", err)
	}
	out := make([]Baseline, 0, len(cms.Items))
	for i := range cms.Items {
		cm := &cms.Items[i]
		raw, ok := cm.Data[configMapDataKey]
		if !ok || raw == "" {
			// Skip malformed entries rather than failing the whole list — a
			// single corrupted ConfigMap shouldn't block warming the others.
			continue
		}
		var bl Baseline
		if err := json.Unmarshal([]byte(raw), &bl); err != nil {
			continue
		}
		out = append(out, bl)
	}
	return out, nil
}
