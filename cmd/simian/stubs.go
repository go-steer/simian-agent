package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newPlanCmd is a placeholder for the autonomous-mode dry-run subcommand
// that lands in M3 (autonomous mode).
func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Run an autonomous-mode planning cycle in dry-run mode (M3)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian plan: not implemented (delivered in M3 — autonomous mode)")
		},
	}
}

// newProvisionCmd is the legacy single-command entry point. It now redirects
// to the split 'arena' / 'sut' commands so existing scripts get a clear hint.
func newProvisionCmd() *cobra.Command {
	return &cobra.Command{
		Use:        "provision",
		Short:      "DEPRECATED — use 'simian arena' (M2 Part A) and 'simian sut' (M2 Part B) instead",
		Deprecated: "split into 'simian arena' (eligibility setup, shipped in M2 Part A) and 'simian sut' (workload + baseline, shipped in M2 Part B)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian provision: split into 'simian arena <create|destroy|describe>' (M2 Part A) and 'simian sut <deploy|destroy>' (M2 Part B)")
		},
	}
}

// newEvaluateCmd is a placeholder for the external-harness driver that lands in M5.
func newEvaluateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "evaluate",
		Short: "Drive an external evaluation harness against scenario records (M5)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian evaluate: not implemented (delivered in M5 — scenario data export)")
		},
	}
}
