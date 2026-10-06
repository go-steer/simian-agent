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

// Package metrics exposes what the controller does as Prometheus metrics.
//
// The metrics are counted from the audit events, not alongside them: the
// Recorder is one more simian.Auditor in the controller's chain, so a
// dashboard and the audit trail cannot disagree about what happened.
package metrics

import (
	"context"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// ActiveFaults lists the faults the controller holds leases for.
// *executor.Executor satisfies it.
type ActiveFaults interface {
	ListActive(ctx context.Context, namespace string) ([]simian.ActiveFault, error)
}

// maxTracked bounds how many faults the Recorder remembers the namespace and
// kind of, between the event that names them and the ones that do not.
const maxTracked = 10000

type faultInfo struct{ namespace, kind string }

// Recorder counts audit events into Prometheus metrics. Safe for concurrent
// use.
type Recorder struct {
	reg *prometheus.Registry

	applied   *prometheus.CounterVec
	ended     *prometheus.CounterVec
	recovered *prometheus.CounterVec
	cycles    *prometheus.CounterVec

	mu     sync.Mutex
	faults map[string]faultInfo
	order  []string
}

// New builds a Recorder with its own registry: the Simian metrics, build
// info, and the Go runtime and process collectors. WatchActive adds the
// active-faults gauge once the executor exists.
func New(version string) *Recorder {
	r := &Recorder{
		reg: prometheus.NewRegistry(),
		applied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simian_faults_applied_total",
			Help: "Faults that reached the cluster, by arena, engine, kind and source (autonomous or directed).",
		}, []string{"namespace", "engine", "kind", "source"}),
		ended: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simian_faults_ended_total",
			Help: "Faults that ended, by arena, kind and outcome (expired, cleared, refused, driver-failed) and the audit reason.",
		}, []string{"namespace", "kind", "outcome", "reason"}),
		recovered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simian_fault_recovery_checks_total",
			Help: "Recovery checks after a fault ended, by arena, kind and whether the workload was Ready again. passed=\"false\" means a fault left something broken.",
		}, []string{"namespace", "kind", "passed"}),
		cycles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simian_cycles_total",
			Help: "Autonomous-mode cycles, by arena and outcome (completed, skipped) and the skip reason.",
		}, []string{"namespace", "outcome", "reason"}),
		faults: map[string]faultInfo{},
	}
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "simian_build_info",
		Help:        "Always 1; the version label is the controller's.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	buildInfo.Set(1)
	r.reg.MustRegister(r.applied, r.ended, r.recovered, r.cycles, buildInfo,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

// WatchActive adds simian_active_faults, read from active at scrape time.
// Separate from New because the Recorder has to be in the auditor chain the
// executor is built with, before the executor exists.
func (r *Recorder) WatchActive(active ActiveFaults) {
	r.reg.MustRegister(&activeCollector{active: active})
}

// Handler serves the metrics in the Prometheus exposition format.
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Emit implements simian.Auditor.
func (r *Recorder) Emit(_ context.Context, e simian.AuditEvent) {
	switch e.Event {
	case audit.EventExecutorReceived, audit.EventDriverApplied:
		info := faultInfo{namespace: firstNamespace(e.Payload), kind: str(e.Payload, "kind")}
		r.remember(e.FaultUID, info)
		if e.Event == audit.EventDriverApplied {
			r.applied.WithLabelValues(info.namespace, str(e.Payload, "engine"), info.kind, string(e.Mode)).Inc()
		}
	case audit.EventExecutorRejected:
		r.end(e, "refused")
	case audit.EventDriverFailed:
		r.end(e, "driver-failed")
	case audit.EventLeaseExpired:
		r.end(e, "expired")
	case audit.EventLeaseCleared:
		if left, _ := e.Payload["left_to_reaper"].(bool); left || e.FaultUID == "" || e.Reason == "driver-clear-failed" {
			return // still in the cluster, or not about one fault
		}
		r.end(e, "cleared")
	case audit.EventFaultRecovered:
		passed, _ := e.Payload["passed"].(bool)
		info := r.lookup(e.FaultUID)
		r.recovered.WithLabelValues(info.namespace, info.kind, boolLabel(passed)).Inc()
	case audit.EventCycleCompleted:
		r.cycles.WithLabelValues(str(e.Payload, "namespace"), audit.CycleCompleted, "").Inc()
	case audit.EventCycleSkipped:
		r.cycles.WithLabelValues(str(e.Payload, "namespace"), audit.CycleSkipped, e.Reason).Inc()
	}
}

func (r *Recorder) end(e simian.AuditEvent, outcome string) {
	if e.FaultUID == "" {
		return
	}
	info := r.lookup(e.FaultUID)
	r.ended.WithLabelValues(info.namespace, info.kind, outcome, e.Reason).Inc()
}

func (r *Recorder) remember(uid string, info faultInfo) {
	if uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.faults[uid]; ok {
		// driver.applied after executor.received: keep what is known.
		if info.namespace == "" {
			info.namespace = prev.namespace
		}
		if info.kind == "" {
			info.kind = prev.kind
		}
		r.faults[uid] = info
		return
	}
	r.faults[uid] = info
	r.order = append(r.order, uid)
	if len(r.order) > maxTracked {
		delete(r.faults, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *Recorder) lookup(uid string) faultInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.faults[uid]
}

// activeCollector reports the faults held right now, by arena, at scrape
// time — a gauge that cannot drift from the lease registry.
type activeCollector struct{ active ActiveFaults }

var activeDesc = prometheus.NewDesc("simian_active_faults",
	"Faults the controller holds a lease for right now, by arena.", []string{"namespace"}, nil)

func (c *activeCollector) Describe(ch chan<- *prometheus.Desc) { ch <- activeDesc }

func (c *activeCollector) Collect(ch chan<- prometheus.Metric) {
	faults, err := c.active.ListActive(context.Background(), "")
	if err != nil {
		return
	}
	by := map[string]float64{}
	for _, f := range faults {
		for _, ns := range f.Manifest.TargetNamespaces() {
			by[ns]++
		}
	}
	for ns, n := range by {
		ch <- prometheus.MustNewConstMetric(activeDesc, prometheus.GaugeValue, n, ns)
	}
}

func firstNamespace(p map[string]any) string {
	switch ts := p["targets"].(type) {
	case []any:
		for _, t := range ts {
			if m, ok := t.(map[string]any); ok {
				if ns, _ := m["namespace"].(string); ns != "" {
					return ns
				}
			}
		}
	case []map[string]any:
		for _, m := range ts {
			if ns, _ := m["namespace"].(string); ns != "" {
				return ns
			}
		}
	}
	return ""
}

func str(p map[string]any, k string) string { s, _ := p[k].(string); return s }

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
