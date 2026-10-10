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

package chaosmesh

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Defaults for ConfirmInjected. Chaos Mesh usually reports a fault injected
// within a couple of seconds; the bound is generous because IOChaos and
// StressChaos enter the target's namespaces first, and because a fault that
// has not landed after this long is not worth keeping either way.
const (
	defaultConfirmTimeout  = 30 * time.Second
	defaultConfirmInterval = time.Second
)

// WithConfirmTimeout bounds how long ConfirmInjected waits for Chaos Mesh to
// report a fault injected. Zero restores the default. Returns d, for chaining
// onto New.
func (d *Driver) WithConfirmTimeout(timeout time.Duration) *Driver {
	d.confirmTimeout = timeout
	return d
}

// ConfirmInjected implements simian.InjectionConfirmer by reading the CR's
// status until Chaos Mesh reports every selected target injected.
//
// It exists because Apply succeeding says nothing about the fault. In a
// 39-hour trial a DNSChaos was created, failed on every pod with
// "/etc/resolv.conf.chaos.bak: No such file or directory", was retried by the
// controller until its duration ran out, and was recorded by Simian as having
// run. The failure was in the CR's status the whole time.
func (d *Driver) ConfirmInjected(ctx context.Context, engineUIDStr string) (string, error) {
	ns, name, gvr, err := decodeEngineUID(engineUIDStr)
	if err != nil {
		return "", err
	}
	timeout, interval := d.confirmTimeout, d.confirmInterval
	if timeout <= 0 {
		timeout = defaultConfirmTimeout
	}
	if interval <= 0 {
		interval = defaultConfirmInterval
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	tick := time.NewTicker(interval)
	defer tick.Stop()

	observed := "status not read yet"
	read := false // whether observed is a status Chaos Mesh reported
	for {
		obj, err := d.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			var done bool
			done, observed = injectionState(obj)
			read = true
			if done {
				return observed, nil
			}
		case apierrors.IsNotFound(err):
			// The object went away: that is the news, whatever it said before.
			observed, read = "get: "+err.Error(), false
		case ctx.Err() == nil && !read:
			// A read that failed only replaces a status nobody has seen yet.
			// Near the deadline client-go's rate limiter refuses requests it
			// predicts would miss it ("Wait(n=1) would exceed context
			// deadline") before the context has expired; reporting that in
			// place of the engine's last word lost the reason the fault did
			// not take (#201).
			observed = "get: " + err.Error()
		}

		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				// The caller gave up, not the timeout: say so, or a shutdown
				// reads as an injection that failed.
				return observed, fmt.Errorf("chaos-mesh: %s/%s: stopped waiting after %s (%w): %s",
					ns, name, time.Since(start).Truncate(time.Second), err, observed)
			}
			return observed, fmt.Errorf("chaos-mesh: %s/%s not injected after %s: %s", ns, name, timeout, observed)
		case <-tick.C:
		}
	}
}

// injectionState reads a Chaos Mesh CR's status and reports whether every
// selected target has been injected, with a line describing what it saw.
//
// Two ways to be done. AllInjected=True is the normal one. The other is every
// record showing a successful Apply: a fault shorter than the polling interval
// can inject and recover between two reads, after which AllInjected is False
// again and only the records remember it happened.
//
// Not done is everything else, including no status at all: the controller has
// not reconciled the object yet, and a fault it never gets to is exactly the
// kind this check is for.
func injectionState(obj *unstructured.Unstructured) (bool, string) {
	conds := map[string]string{}
	list, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range list {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		conds[t] = s
	}

	records, _, _ := unstructured.NestedSlice(obj.Object, "status", "experiment", "containerRecords")
	applied := 0
	var failures []string
	for _, r := range records {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, _ := rm["id"].(string)
		ok, lastFailure := recordApplied(rm)
		if ok {
			applied++
		} else if lastFailure != "" {
			failures = append(failures, id+": "+lastFailure)
		}
	}

	if conds["AllInjected"] == "True" {
		return true, fmt.Sprintf("AllInjected=True, %d target(s)", len(records))
	}
	if len(records) > 0 && applied == len(records) {
		return true, fmt.Sprintf("all %d target(s) show a successful apply", len(records))
	}

	switch {
	case len(failures) > 0:
		return false, fmt.Sprintf("%d of %d target(s) injected; %s", applied, len(records), strings.Join(failures, "; "))
	case conds["Selected"] == "False":
		return false, "Selected=False: the selector matched no target"
	case len(conds) == 0:
		return false, "no status conditions yet: the Chaos Mesh controller has not reconciled this object"
	default:
		return false, fmt.Sprintf("%d of %d target(s) injected, no failure reported yet", applied, len(records))
	}
}

// recordApplied reports whether a container record shows a successful Apply,
// and otherwise the message of its most recent failed one.
func recordApplied(record map[string]any) (bool, string) {
	events, _, _ := unstructured.NestedSlice(record, "events")
	lastFailure := ""
	for _, e := range events {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if op, _ := em["operation"].(string); op != "Apply" {
			continue
		}
		switch t, _ := em["type"].(string); t {
		case "Succeeded":
			return true, ""
		case "Failed":
			lastFailure, _ = em["message"].(string)
			if lastFailure == "" {
				lastFailure = "apply failed"
			}
		}
	}
	return false, lastFailure
}
