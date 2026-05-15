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
	"fmt"

	"github.com/spf13/cobra"
)

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
