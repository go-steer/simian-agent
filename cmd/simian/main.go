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

// Command simian is the single Simian Agent binary, hosting the controller
// (`simian serve`), the directed-mode CLI client (`simian chaos`), and
// stub subcommands for milestones not yet shipped (`simian plan`, `simian
// provision`, `simian evaluate`).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0-dev"

func main() {
	root := &cobra.Command{
		Use:           "simian",
		Short:         "Simian Agent — AI-native chaos engineering orchestrator",
		Long:          "Simian Agent applies controlled chaos to eligible Kubernetes namespaces. It accepts plain-text intent (LLM-translated) or hand-built FaultManifests (deterministic-control path).",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newChaosCmd())
	root.AddCommand(newArenaCmd())
	root.AddCommand(newSutCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newBaselineCmd())
	root.AddCommand(newWatchCmd())
	root.AddCommand(newProvisionCmd())
	root.AddCommand(newEvaluateCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
