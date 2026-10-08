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

package loop

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Settings is what an operator may change about autonomous mode while the
// controller runs: whether it runs, where, how often, and how much each
// cycle may do. The executor's limits are not here — they are the ceilings
// these must fit under, and are set where the controller is installed.
type Settings struct {
	Enabled             bool                   `json:"enabled"`
	Namespaces          []string               `json:"namespaces"`
	Interval            time.Duration          `json:"-"`
	MaxFaultsPerCycle   int                    `json:"max_faults_per_cycle"`
	MaxSeverityPerCycle simian.BlastRadiusTier `json:"max_severity_per_cycle"`
	Hypothesis          string                 `json:"hypothesis,omitempty"`
	// Paused namespaces keep their place but run no cycles; "*" pauses all.
	Paused []string `json:"paused,omitempty"`
}

// PauseAll in Settings.Paused pauses every namespace.
const PauseAll = "*"

func (s Settings) MarshalJSON() ([]byte, error) {
	type plain Settings
	return json.Marshal(struct {
		plain
		Interval string `json:"interval"`
	}{plain(s), s.Interval.String()})
}

func (s *Settings) UnmarshalJSON(b []byte) error {
	type plain Settings
	var v struct {
		plain
		Interval string `json:"interval"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = Settings(v.plain)
	if v.Interval != "" {
		d, err := time.ParseDuration(v.Interval)
		if err != nil {
			return fmt.Errorf("interval: %w", err)
		}
		s.Interval = d
	}
	return nil
}

// IsPaused reports whether ns runs no cycles.
func (s Settings) IsPaused(ns string) bool {
	return slices.Contains(s.Paused, PauseAll) || slices.Contains(s.Paused, ns)
}

// Limits are the ceilings Settings must fit under: the executor's, and
// which namespaces are arenas.
type Limits struct {
	MaxConcurrentFaults int
	PermittedTiers      []simian.BlastRadiusTier
	Arenas              []string
	// MinInterval is the shortest cycle interval accepted.
	MinInterval time.Duration
	// Install is the install's own settings. A value equal to the
	// install's is accepted as it is: an operator who only pauses must not
	// be refused for a limit the install itself chose to exceed.
	Install *Settings
}

// MinInterval is the shortest cycle interval an operator may set: each
// cycle is an LLM call, and a fault needs time to be watched.
const MinInterval = time.Minute

// Validate refuses settings that would exceed lim.
func (s Settings) Validate(lim Limits) error {
	if lim.MinInterval <= 0 {
		lim.MinInterval = MinInterval
	}
	if s.Enabled && len(s.Namespaces) == 0 {
		return fmt.Errorf("autonomous mode needs at least one namespace")
	}
	inst := Settings{}
	if lim.Install != nil {
		inst = *lim.Install
	}
	for _, ns := range s.Namespaces {
		if !slices.Contains(lim.Arenas, ns) && !slices.Contains(inst.Namespaces, ns) {
			return fmt.Errorf("namespace %q is not an arena", ns)
		}
	}
	if s.Interval < lim.MinInterval && s.Interval != inst.Interval {
		return fmt.Errorf("interval %s is under the minimum %s", s.Interval, lim.MinInterval)
	}
	ownFaults := lim.Install != nil && s.MaxFaultsPerCycle == inst.MaxFaultsPerCycle
	switch {
	case ownFaults:
	case s.MaxFaultsPerCycle < 1:
		return fmt.Errorf("max faults per cycle must be at least 1")
	case lim.MaxConcurrentFaults > 0 && s.MaxFaultsPerCycle > lim.MaxConcurrentFaults:
		return fmt.Errorf("max faults per cycle %d is over the executor's max concurrent faults %d (executor.maxConcurrentFaults)",
			s.MaxFaultsPerCycle, lim.MaxConcurrentFaults)
	}
	ownTier := lim.Install != nil && s.MaxSeverityPerCycle == inst.MaxSeverityPerCycle
	if !ownTier && s.MaxSeverityPerCycle != "" && len(lim.PermittedTiers) > 0 && !slices.Contains(lim.PermittedTiers, s.MaxSeverityPerCycle) {
		return fmt.Errorf("severity cap %q is not a permitted tier %v (executor.permittedTiers)", s.MaxSeverityPerCycle, lim.PermittedTiers)
	}
	return nil
}

// Source says where the current settings came from.
type Source struct {
	// By is the person who set them from the UI; empty for the install's
	// defaults.
	By string    `json:"by,omitempty"`
	At time.Time `json:"at,omitzero"`
}

// Control holds the autonomous settings the loop reads at each cycle, and
// tells it when they change. Safe for concurrent use.
type Control struct {
	mu       sync.Mutex
	settings Settings
	defaults Settings
	source   Source
	changed  chan struct{}
}

// NewControl starts from defaults, the install's settings.
func NewControl(defaults Settings) *Control {
	return &Control{settings: defaults, defaults: defaults, changed: make(chan struct{}, 1)}
}

// Get returns the current settings and where they came from.
func (c *Control) Get() (Settings, Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clone(c.settings), c.source
}

// Defaults returns the install's settings.
func (c *Control) Defaults() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clone(c.defaults)
}

// Set replaces the settings, records who set them, and wakes the loop. It
// does not validate; callers check against Limits first.
func (c *Control) Set(s Settings, src Source) {
	c.mu.Lock()
	c.settings = clone(s)
	c.source = src
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// Changed is signalled after each Set.
func (c *Control) Changed() <-chan struct{} { return c.changed }

func clone(s Settings) Settings {
	s.Namespaces = slices.Clone(s.Namespaces)
	s.Paused = slices.Clone(s.Paused)
	return s
}
