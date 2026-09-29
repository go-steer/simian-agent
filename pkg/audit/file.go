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

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Record is one audit event as written to a file, and as read back from
// either that file or the controller's JSON log: the SLogAuditor's lines
// carry the same keys, plus slog's own, which are ignored.
type Record struct {
	TS         time.Time      `json:"ts"`
	Event      string         `json:"event"`
	FaultUID   string         `json:"fault_uid,omitempty"`
	PlanID     string         `json:"plan_id,omitempty"`
	ScenarioID string         `json:"scenario_id,omitempty"`
	Mode       string         `json:"mode,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// DefaultFileMaxBytes is where FileAuditor rotates when no limit is given.
// The 39-hour GKE trial wrote about 20MB of audit events.
const DefaultFileMaxBytes = 100 << 20

// FileAuditor appends audit events to a file, one JSON object per line, so
// the trail outlives the controller process and its pod's log buffer.
//
// When the file passes MaxBytes it is renamed to PATH.1, replacing any
// previous PATH.1, and a new file is started. Two generations is enough to
// answer "what happened since yesterday" without an unbounded volume.
type FileAuditor struct {
	path     string
	maxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
	// onError is told when a write fails. An audit sink that cannot write
	// must not take the controller down, and must not fail silently either.
	onError func(error)
}

// OpenFile opens (or creates) path for appending. maxBytes ≤ 0 means
// DefaultFileMaxBytes. onError may be nil.
func OpenFile(path string, maxBytes int64, onError func(error)) (*FileAuditor, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultFileMaxBytes
	}
	a := &FileAuditor{path: path, maxBytes: maxBytes, onError: onError}
	if err := a.open(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *FileAuditor) open() error {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", a.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("audit: stat %s: %w", a.path, err)
	}
	a.f, a.size = f, st.Size()
	return nil
}

// Paths returns the files the auditor writes, oldest first: what a reader
// needs to see the whole retained trail.
func (a *FileAuditor) Paths() []string { return []string{a.path + ".1", a.path} }

// Emit implements simian.Auditor.
func (a *FileAuditor) Emit(ctx context.Context, ev simian.AuditEvent) {
	if ev.ScenarioID == "" {
		ev.ScenarioID = ScenarioIDFrom(ctx)
	}
	line, err := json.Marshal(Record{
		TS:         time.Now().UTC(),
		Event:      ev.Event,
		FaultUID:   ev.FaultUID,
		PlanID:     ev.PlanID,
		ScenarioID: ev.ScenarioID,
		Mode:       string(ev.Mode),
		Reason:     ev.Reason,
		Payload:    ev.Payload,
	})
	if err != nil {
		a.fail(fmt.Errorf("audit: encode %s: %w", ev.Event, err))
		return
	}
	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return
	}
	if a.size > 0 && a.size+int64(len(line)) > a.maxBytes {
		if err := a.rotate(); err != nil {
			a.fail(err)
			if a.f == nil {
				return
			}
		}
	}
	n, err := a.f.Write(line)
	a.size += int64(n)
	if err != nil {
		a.fail(fmt.Errorf("audit: write %s: %w", a.path, err))
	}
}

func (a *FileAuditor) rotate() error {
	if err := a.f.Close(); err != nil {
		a.fail(fmt.Errorf("audit: close %s: %w", a.path, err))
	}
	a.f = nil
	if err := os.Rename(a.path, a.path+".1"); err != nil {
		// Keep writing to the old file rather than lose events.
		if oerr := a.open(); oerr != nil {
			return oerr
		}
		return fmt.Errorf("audit: rotate %s: %w", a.path, err)
	}
	return a.open()
}

func (a *FileAuditor) fail(err error) {
	if a.onError != nil {
		a.onError(err)
	}
}

// Close flushes and closes the file.
func (a *FileAuditor) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// Multi fans each event out to every auditor in order.
type Multi []simian.Auditor

// Emit implements simian.Auditor.
func (m Multi) Emit(ctx context.Context, ev simian.AuditEvent) {
	for _, a := range m {
		a.Emit(ctx, ev)
	}
}
