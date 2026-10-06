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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/audit"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the controller's audit trail",
	}
	cmd.AddCommand(newAuditExportCmd())
	return cmd
}

func newAuditExportCmd() *cobra.Command {
	var (
		since  string
		format string
		cycles bool
	)
	cmd := &cobra.Command{
		Use:   "export [FILE...]",
		Short: "One row per fault: what was asked for, what ran, on what, and how it ended",
		Long: `Folds audit events into one row per fault.

Reads the files a 'simian serve --audit-file' writes (pass <file>.1 first to
include the previous generation), or the controller's own log, from stdin when
no file is given:

  kubectl -n simian-system logs deploy/simian-controller | simian audit export --since 24h

The controller image has no shell or cat, so to read the file in the pod, run
export there:

  kubectl -n simian-system exec deploy/simian-controller -- simian audit export /var/lib/simian/audit.jsonl

With --cycles, one row per autonomous-mode cycle instead: whether it
completed or was skipped and why, the hypothesis, the plan's steps, the faults
it applied and the steps that were refused.

Lines that are not audit events are skipped.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var cutoff time.Time
			if since != "" {
				t, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				cutoff = t
			}
			var records []audit.Record
			if len(args) == 0 {
				recs, err := audit.ReadRecords(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				records = recs
			}
			for _, path := range args {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				recs, err := audit.ReadRecords(f)
				_ = f.Close()
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
				records = append(records, recs...)
			}
			// Generations and log streams can arrive out of order.
			sort.SliceStable(records, func(i, j int) bool { return records[i].TS.Before(records[j].TS) })
			if cycles {
				rows := audit.Cycles(records)
				if !cutoff.IsZero() {
					kept := rows[:0:0]
					for _, r := range rows {
						if !r.StartedAt.Before(cutoff) {
							kept = append(kept, r)
						}
					}
					rows = kept
				}
				switch format {
				case "json":
					enc := json.NewEncoder(cmd.OutOrStdout())
					for _, r := range rows {
						if err := enc.Encode(r); err != nil {
							return err
						}
					}
					return nil
				case "table":
					return writeCyclesTable(cmd.OutOrStdout(), rows)
				default:
					return fmt.Errorf("--format must be table or json, not %q", format)
				}
			}
			rows := audit.Faults(records)
			if !cutoff.IsZero() {
				rows = audit.Since(rows, cutoff)
			}
			switch format {
			case "json":
				return writeFaultsJSON(cmd.OutOrStdout(), rows)
			case "table":
				return writeFaultsTable(cmd.OutOrStdout(), rows)
			default:
				return fmt.Errorf("--format must be table or json, not %q", format)
			}
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Only faults first seen after this: a duration back from now (24h) or an RFC 3339 time")
	cmd.Flags().StringVar(&format, "format", "table", "table, or json for one JSON object per line with the full spec and targets")
	cmd.Flags().BoolVar(&cycles, "cycles", false, "One row per autonomous-mode cycle — outcome, skip reason, hypothesis, plan, applied and refused steps — instead of per fault")
	return cmd
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a duration nor an RFC 3339 time", s)
}

func writeFaultsJSON(w io.Writer, rows []audit.FaultRow) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func writeFaultsTable(w io.Writer, rows []audit.FaultRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FAULT\tSTARTED\tSOURCE\tKIND\tTARGETS\tOUTCOME\tREASON\tENDED\tINJECTED\tEFFICACY\tRECOVERED\tSPEC")
	for _, r := range rows {
		started := r.AppliedAt
		if started.IsZero() {
			started = r.ReceivedAt
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.FaultUID, stamp(started), dash(r.Source), dash(r.Kind), targetsCell(r.Targets),
			r.Outcome, dash(r.Reason), stamp(r.EndedAt), verdict(r.Injected), verdict(r.Efficacy), verdict(r.Recovered), specCell(r.Spec))
	}
	return tw.Flush()
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func verdict(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "yes"
	default:
		return "no"
	}
}

func targetsCell(targets []any) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		m, _ := t.(map[string]any)
		ns, _ := m["namespace"].(string)
		switch {
		case m["name"] != nil:
			parts = append(parts, fmt.Sprintf("%s/%v", ns, m["name"]))
		case m["labels"] != nil:
			labels, _ := m["labels"].(map[string]any)
			kv := make([]string, 0, len(labels))
			for k, v := range labels {
				kv = append(kv, fmt.Sprintf("%s=%v", k, v))
			}
			sort.Strings(kv)
			parts = append(parts, ns+"/"+strings.Join(kv, ","))
		default:
			parts = append(parts, ns)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// specCell keeps the table to one line per fault; --format json has the
// whole spec.
func specCell(spec any) string {
	if spec == nil {
		return "-"
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return "?"
	}
	const limit = 120
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

func writeCyclesTable(w io.Writer, rows []audit.CycleRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STARTED\tNAMESPACE\tOUTCOME\tREASON\tPLAN\tAPPLIED\tREFUSED\tHYPOTHESIS / DETAIL")
	for _, r := range rows {
		steps := make([]string, 0, len(r.Steps))
		for _, s := range r.Steps {
			steps = append(steps, fmt.Sprintf("%s→%s %s", dash(s.Kind), dash(s.Target), s.Duration))
		}
		text := r.Hypothesis
		if text == "" {
			text = r.Detail
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			stamp(r.StartedAt), r.Namespace, r.Outcome, dash(r.Reason), dash(strings.Join(steps, "; ")),
			len(r.Applied), len(r.Refused), truncate(text, 100))
	}
	return tw.Flush()
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return dash(s)
	}
	return s[:n-1] + "…"
}
