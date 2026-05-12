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
	root.AddCommand(newPlanCmd())
	root.AddCommand(newProvisionCmd())
	root.AddCommand(newEvaluateCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
