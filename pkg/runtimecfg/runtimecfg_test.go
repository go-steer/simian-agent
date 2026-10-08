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

package runtimecfg

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/pkg/loop"
)

func TestSettingsAreKeptAndForgotten(t *testing.T) {
	ctx := context.Background()
	s := Store{Client: fake.NewClientset(), Namespace: "simian-system"}
	if r, err := s.Load(ctx); err != nil || r != nil {
		t.Fatalf("nothing kept yet: %+v, %v", r, err)
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	want := Record{
		Settings: loop.Settings{Enabled: true, Namespaces: []string{"boutique"}, Interval: 5 * time.Minute, MaxFaultsPerCycle: 1},
		Source:   loop.Source{By: "alice@example.com", At: at},
	}
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	want.Settings.Interval = 10 * time.Minute
	if err := s.Save(ctx, want); err != nil { // an update, not a second create
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil || got == nil || got.Settings.Interval != 10*time.Minute || got.Source.By != "alice@example.com" || !got.Source.At.Equal(at) {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Load(ctx); err != nil || r != nil {
		t.Errorf("after Clear: %+v, %v", r, err)
	}
}
