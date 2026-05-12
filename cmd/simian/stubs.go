package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newPlanCmd is a placeholder for the autonomous-mode dry-run subcommand
// that lands in M4.
func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Run an autonomous-mode planning cycle in dry-run mode (M4)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian plan: not implemented in M1 (delivered in M4)")
		},
	}
}

// newProvisionCmd is a placeholder for the namespace lifecycle subcommand
// that lands in M3. M1 assumes operator-managed eligibility.
func newProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Manage eligible namespaces and SUT lifecycle (M3)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "deploy",
		Short: "Provision a fresh eligible namespace + SUT (M3)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian provision deploy: not implemented in M1 (delivered in M3)")
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "cleanup",
		Short: "Tear down a provisioned namespace (M3)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian provision cleanup: not implemented in M1 (delivered in M3)")
		},
	})
	return cmd
}

// newEvaluateCmd is a placeholder for the external-harness driver that lands in M6.
func newEvaluateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "evaluate",
		Short: "Drive an external evaluation harness against scenario records (M6)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("simian evaluate: not implemented in M1 (delivered in M6)")
		},
	}
}
