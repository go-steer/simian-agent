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
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "http://localhost:8081/sse", "Simian MCP/SSE endpoint URL")
	cmd.Flags().StringVar(&ns, "namespace", "", "Namespace to watch (required)")
	cmd.Flags().DurationVar(&pollInterval, "interval", 3*time.Second, "Poll interval")
	cmd.Flags().IntVar(&limit, "recent", 8, "Number of recent faults to show")
	return cmd
}

// watchSnapshot captures one poll cycle's worth of data — the two
// pieces of state we render — plus the wall-clock time we captured it,
// used for the deadline countdown math.
type watchSnapshot struct {
	Namespace string
	CapturedAt time.Time
	Active    []activeFault
	Recent    []recentFault
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
	snap := &watchSnapshot{Namespace: ns, CapturedAt: time.Now()}
	// Empty registry serializes to "null" (json.Marshal on nil slice) —
	// let json.Unmarshal handle it as an empty slice.
	if activeText != "" && activeText != "null" {
		if err := json.Unmarshal([]byte(activeText), &snap.Active); err != nil {
			return nil, fmt.Errorf("parse active faults: %w", err)
		}
	}
	if recentText != "" && recentText != "null" {
		if err := json.Unmarshal([]byte(recentText), &snap.Recent); err != nil {
			return nil, fmt.Errorf("parse recent faults: %w", err)
		}
	}
	return snap, nil
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
