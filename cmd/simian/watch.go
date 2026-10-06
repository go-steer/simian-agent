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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/audit"
)

func newWatchCmd() *cobra.Command {
	var (
		mcpURL       string
		ns           string
		pollInterval time.Duration
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Live terminal view of what the controller is doing in a namespace",
		Long: `Connects to the simian serve MCP endpoint and renders a live view
of the namespace's active faults + recent history. Polls on interval
(default 3s). Ctrl-C to exit.

Replaces the "tail -f serve.log | jq" pattern when you just want to see
what the autonomous loop is up to right now.

Examples:
  simian watch --namespace boutique-m3
  simian watch --namespace boutique-m3 --interval 5s --recent 20`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			// Watch is long-running; use the command's own context (which
			// Cobra already wires to Ctrl-C via signal.NotifyContext).
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			cli, err := newMCPClient(ctx, mcpURL)
			if err != nil {
				return err
			}
			defer func() { _ = cli.Close() }()

			// First render immediately, then every pollInterval.
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				snapshot, err := gatherSnapshot(ctx, cli, ns, limit)
				if err != nil {
					// A single failed poll shouldn't tear down the watch — the
					// controller may be restarting, or the network hiccuped.
					// Render the error and keep going.
					clearScreen(os.Stdout)
					fmt.Fprintf(os.Stdout, "simian watch — %s\npoll error: %v\n(will retry in %s)\n", ns, err, pollInterval)
				} else {
					renderSnapshot(os.Stdout, snapshot)
				}
				select {
				case <-ctx.Done():
					// Newline so the operator's shell prompt lands on its own line.
					fmt.Fprintln(os.Stdout)
					return nil
				case <-ticker.C:
				}
			}
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp-url", defaultMCPURL(), "Simian MCP/SSE endpoint URL ($SIMIAN_MCP_URL, else http://localhost:8081/sse)")
	cmd.Flags().StringVar(&ns, "namespace", "", "Namespace to watch (required)")
	cmd.Flags().DurationVar(&pollInterval, "interval", 3*time.Second, "Poll interval")
	cmd.Flags().IntVar(&limit, "recent", 8, "Number of recent faults to show")
	return cmd
}

// watchSnapshot captures one poll cycle's worth of data — the two
// pieces of state we render — plus the wall-clock time we captured it,
// used for the deadline countdown math.
type watchSnapshot struct {
	Namespace  string
	CapturedAt time.Time
	Active     []activeFault
	Recent     []recentFault
	Cycles     []audit.CycleRow
	// CyclesNote says why there are no cycles to show when the controller
	// cannot report them (one older than get_recent_cycles).
	CyclesNote string
}

// activeFault mirrors simian.ActiveFault but is redeclared locally so
// cmd/simian doesn't take an import on pkg/simian just for JSON shape.
type activeFault struct {
	FaultUID string `json:"fault_uid"`
	Manifest struct {
		Engine       string `json:"engine"`
		ResourceKind string `json:"resource_kind"`
		Targets      []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"targets"`
	} `json:"manifest"`
	AppliedAt time.Time `json:"applied_at"`
	Deadline  time.Time `json:"deadline"`
}

// recentFault mirrors executor.RecentFault, same reason as activeFault.
type recentFault struct {
	FaultUID string `json:"fault_uid"`
	Manifest struct {
		Engine       string `json:"engine"`
		ResourceKind string `json:"resource_kind"`
		Targets      []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"targets"`
	} `json:"manifest"`
	AppliedAt   time.Time `json:"applied_at"`
	ClearedAt   time.Time `json:"cleared_at,omitempty"`
	ClearReason string    `json:"clear_reason,omitempty"`
}

func gatherSnapshot(ctx context.Context, cli *mcpclient.Client, ns string, limit int) (*watchSnapshot, error) {
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	activeText, err := callToolText(pollCtx, cli, "list_active_faults", map[string]any{"namespace": ns})
	if err != nil {
		return nil, fmt.Errorf("list_active_faults: %w", err)
	}
	recentText, err := callToolText(pollCtx, cli, "get_recent_faults", map[string]any{"namespace": ns, "limit": float64(limit)})
	if err != nil {
		return nil, fmt.Errorf("get_recent_faults: %w", err)
	}
	cyclesText, cyclesErr := callToolText(pollCtx, cli, "get_recent_cycles", map[string]any{"namespace": ns, "limit": float64(5)})
	snap, err := parseSnapshot(ns, activeText, recentText, cyclesText)
	if err != nil {
		return nil, err
	}
	if cyclesErr != nil {
		snap.CyclesNote = "this controller does not report cycles (older than v0.1.14)"
	}
	snap.CapturedAt = time.Now()
	return snap, nil
}

// parseSnapshot reads the three tools' answers. get_recent_faults and
// get_recent_cycles wrap their lists in {"enabled":…,"recent"/"cycles":[…]};
// watch used to read get_recent_faults as a bare list and failed on every
// poll, so both shapes are accepted.
func parseSnapshot(ns, activeText, recentText, cyclesText string) (*watchSnapshot, error) {
	snap := &watchSnapshot{Namespace: ns}
	// Empty registry serializes to "null" (json.Marshal on nil slice) —
	// let json.Unmarshal handle it as an empty slice.
	if activeText != "" && activeText != "null" {
		if err := json.Unmarshal([]byte(activeText), &snap.Active); err != nil {
			return nil, fmt.Errorf("parse active faults: %w", err)
		}
	}
	if err := unwrapList(recentText, "recent", &snap.Recent); err != nil {
		return nil, fmt.Errorf("parse recent faults: %w", err)
	}
	if err := unwrapList(cyclesText, "cycles", &snap.Cycles); err != nil {
		return nil, fmt.Errorf("parse recent cycles: %w", err)
	}
	return snap, nil
}

// unwrapList decodes text, a JSON list or an object holding the list under
// key, into out. Empty text and null leave out empty.
func unwrapList(text, key string, out any) error {
	text = strings.TrimSpace(text)
	if text == "" || text == "null" {
		return nil
	}
	if strings.HasPrefix(text, "[") {
		return json.Unmarshal([]byte(text), out)
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &wrapper); err != nil {
		return err
	}
	raw, ok := wrapper[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// callToolText invokes an MCP tool and returns the first text-content
// block from the response. Errors on tool-level errors or missing text.
// Local variant of callTool() (in chaos.go) that returns the string
// instead of printing it, so the caller can parse the JSON payload.
func callToolText(ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) (string, error) {
	req := mcpsdk.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("tool %s returned error", name)
	}
	for _, c := range res.Content {
		if tc, ok := mcpsdk.AsTextContent(c); ok {
			return tc.Text, nil
		}
	}
	return "", nil
}

// ANSI: clear screen + move cursor home. Portable across xterm-family
// terminals; skips fancy alternate-screen buffer since we want the
// operator to be able to scroll back through their shell after quitting.
const ansiClearHome = "\033[2J\033[H"

func clearScreen(w io.Writer) { fmt.Fprint(w, ansiClearHome) }

func renderSnapshot(w io.Writer, s *watchSnapshot) {
	clearScreen(w)
	fmt.Fprintf(w, "simian watch — %s   (updated %s)\n", s.Namespace, s.CapturedAt.Format("15:04:05"))
	fmt.Fprintln(w, strings.Repeat("─", 72))
	renderActive(w, s.Active, s.CapturedAt)
	fmt.Fprintln(w)
	renderCycles(w, s.Cycles, s.CyclesNote)
	fmt.Fprintln(w)
	renderRecent(w, s.Recent)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "(Ctrl-C to exit)")
}

func renderActive(w io.Writer, active []activeFault, now time.Time) {
	fmt.Fprintf(w, "ACTIVE FAULTS (%d)\n", len(active))
	if len(active) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, f := range active {
		remaining := f.Deadline.Sub(now).Round(time.Second)
		target := "?"
		if len(f.Manifest.Targets) > 0 {
			target = f.Manifest.Targets[0].Name
		}
		fmt.Fprintf(w, "  %s  %s %-16s → %-20s  [%s remaining]\n",
			shortUID(f.FaultUID),
			padEngine(f.Manifest.Engine),
			f.Manifest.ResourceKind,
			target,
			formatRemaining(remaining),
		)
	}
}

func renderRecent(w io.Writer, recent []recentFault) {
	fmt.Fprintf(w, "RECENT FAULTS (%d)\n", len(recent))
	if len(recent) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, f := range recent {
		status := "applied"
		if !f.ClearedAt.IsZero() {
			status = "cleared (" + f.ClearReason + ")"
		}
		target := "?"
		if len(f.Manifest.Targets) > 0 {
			target = f.Manifest.Targets[0].Name
		}
		ts := f.AppliedAt.Format("15:04:05")
		if !f.ClearedAt.IsZero() {
			ts = f.ClearedAt.Format("15:04:05")
		}
		fmt.Fprintf(w, "  %s  %s %s %-16s → %-20s  %s\n",
			ts,
			shortUID(f.FaultUID),
			padEngine(f.Manifest.Engine),
			f.Manifest.ResourceKind,
			target,
			status,
		)
	}
}

// shortUID trims a ULID-style fault_uid (e.g.
// "f-01KX42VR47D5XZEH65N3BP1VC6") to just enough to distinguish rows
// while keeping the line compact.
func shortUID(uid string) string {
	if len(uid) <= 12 {
		return uid
	}
	return uid[:12]
}

// padEngine right-pads the engine name to the width of the longest
// engine string so the columns line up. Also inserts a leading space
// so single-character brackets on the left don't run together.
func padEngine(engine string) string {
	const width = 16 // room for "network-policy " + a couple more
	if len(engine) >= width {
		return engine
	}
	return engine + strings.Repeat(" ", width-len(engine))
}

// formatRemaining shows a compact "8s" or "2m3s" instead of Go's
// default "8.000...s" — the extra precision is noise in a countdown.
func formatRemaining(d time.Duration) string {
	if d < 0 {
		return "expired"
	}
	return d.String()
}

// renderCycles shows what autonomous mode decided: the latest plan in full —
// hypothesis, steps, why each lasts as long as it does — then a line per
// recent cycle, so a skipped cycle says why without reading the log.
func renderCycles(w io.Writer, cycles []audit.CycleRow, note string) {
	fmt.Fprintf(w, "AUTONOMOUS CYCLES (%d)\n", len(cycles))
	if note != "" {
		fmt.Fprintf(w, "  (%s)\n", note)
		return
	}
	if len(cycles) == 0 {
		fmt.Fprintln(w, "  (none — autonomous mode off, or no cycle yet)")
		return
	}
	for _, c := range cycles { // newest first
		if c.Hypothesis == "" {
			continue
		}
		fmt.Fprintf(w, "  latest plan (%s):\n", c.StartedAt.Local().Format("15:04:05"))
		for _, line := range wrap(c.Hypothesis, 66) {
			fmt.Fprintf(w, "    %s\n", line)
		}
		for _, st := range c.Steps {
			fmt.Fprintf(w, "    %d. %s → %s for %s\n", st.Order, st.Kind, st.Target, st.Duration)
			if st.DurationRationale != "" {
				fmt.Fprintf(w, "       why that long: %s\n", truncate(st.DurationRationale, 60))
			}
		}
		break
	}
	for _, c := range cycles {
		what := ""
		switch c.Outcome {
		case audit.CycleCompleted:
			parts := make([]string, 0, len(c.Steps))
			for _, st := range c.Steps {
				parts = append(parts, st.Kind+"→"+st.Target)
			}
			what = fmt.Sprintf("%s (%d applied", strings.Join(parts, ", "), len(c.Applied))
			if len(c.Refused) > 0 {
				what += fmt.Sprintf(", %d refused: %s", len(c.Refused), truncate(c.Refused[0].Error, 50))
			}
			what += ")"
		case audit.CycleSkipped:
			what = c.Reason
			if c.Detail != "" {
				what += ": " + truncate(c.Detail, 60)
			}
		default:
			what = c.Outcome
		}
		fmt.Fprintf(w, "  %s  %-9s %s\n", c.StartedAt.Local().Format("15:04:05"), c.Outcome, what)
	}
}

// wrap breaks s into lines of at most width runes, on spaces.
func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line != "" && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
