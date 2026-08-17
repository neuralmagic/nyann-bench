package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/spf13/cobra"
)

// compileStarlarkCmd is an internal process-isolation boundary used by the MCP
// service. It intentionally accepts source only on stdin and emits only the
// parsed scenario IR.
func compileStarlarkCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "compile-starlark",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := limitStarlarkCompilerMemory(); err != nil {
				return fmt.Errorf("setting Starlark compiler memory limit: %w", err)
			}
			source, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), config.MaxScenarioInputBytes+1))
			if err != nil {
				return fmt.Errorf("reading Starlark source: %w", err)
			}
			if len(source) > config.MaxScenarioInputBytes {
				return fmt.Errorf("Starlark source exceeds %d bytes", config.MaxScenarioInputBytes)
			}
			scenario, err := config.ParseStarlarkSource("<mcp>", string(source))
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(scenario)
			if err != nil {
				return fmt.Errorf("encoding scenario: %w", err)
			}
			if len(encoded) > config.MaxScenarioInputBytes {
				return fmt.Errorf("compiled scenario exceeds %d bytes", config.MaxScenarioInputBytes)
			}
			if _, err := cmd.OutOrStdout().Write(encoded); err != nil {
				return fmt.Errorf("writing scenario: %w", err)
			}
			return nil
		},
	}
}
