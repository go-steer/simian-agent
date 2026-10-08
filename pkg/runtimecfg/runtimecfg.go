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

// Package runtimecfg keeps the autonomous-mode settings an operator changed
// from the web UI in a ConfigMap, so they survive a controller restart.
//
// The ConfigMap is the controller's, not the chart's: helm does not render
// it, so a helm upgrade leaves an operator's change in place. Deleting it
// returns autonomous mode to the install's settings.
package runtimecfg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/loop"
)

// Name is the ConfigMap's name.
const Name = "simian-runtime-config"

const key = "autonomous.json"

// Record is what is kept: the settings and who set them.
type Record struct {
	Settings loop.Settings `json:"settings"`
	Source   loop.Source   `json:"source"`
}

// Store reads and writes the Record in Namespace.
type Store struct {
	Client    kubernetes.Interface
	Namespace string
}

// Load returns the kept Record, or nil when there is none.
func (s Store) Load(ctx context.Context) (*Record, error) {
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, ok := cm.Data[key]
	if !ok {
		return nil, nil
	}
	var r Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", s.Namespace, Name, err)
	}
	return &r, nil
}

// Save keeps r, creating the ConfigMap if it is not there.
func (s Store) Save(ctx context.Context, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	cms := s.Client.CoreV1().ConfigMaps(s.Namespace)
	cm, err := cms.Get(ctx, Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: s.Namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "simian-controller"}},
			Data: map[string]string{key: string(b)},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = string(b)
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// Clear forgets the kept settings, so the install's apply again.
func (s Store) Clear(ctx context.Context) error {
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	delete(cm.Data, key)
	_, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// OwnNamespace is the namespace the controller runs in, from its service
// account's mount, or fallback outside a cluster.
func OwnNamespace(fallback string) string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return fallback
}
