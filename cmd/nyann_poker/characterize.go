package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/warmup"
	"github.com/spf13/cobra"
)

func characterizeCmd() *cobra.Command {
	var (
		target      string
		model       string
		cfgInput    string
		concurrency int
		outputFile  string
	)

	cmd := &cobra.Command{
		Use:   "characterize",
		Short: "Characterize an inference engine for data-driven warmup",
		Long: `Run a D-optimal concurrency sweep to profile an inference engine.

The output is a JSON profile containing a fitted TPOT(C) = a + b*C + c*C²
model. Use this profile with the "profile" warmup mode in the generate
command to minimize experiment startup time.

Run this once per engine/model/hardware/workload configuration.

Example:
  nyann_poker characterize --target http://localhost:8000/v1 \
    --concurrency 64 --config workload.json --output profile.json

  nyann_poker generate --target http://localhost:8000/v1 \
    --config '{"warmup":{"profile":"profile.json"},...}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			c := client.New(target)
			slog.Info("Waiting for endpoint", "target", target)
			if err := c.WaitForReady(ctx); err != nil {
				return err
			}
			slog.Info("Endpoint ready")

			if model == "" {
				detected, err := c.DetectModel(ctx)
				if err != nil {
					return fmt.Errorf("auto-detecting model (use --model to specify): %w", err)
				}
				model = detected
				slog.Info("Detected model", "model", model)
			}

			cfg, err := config.Parse(cfgInput)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}

			w := cfg.Workload
			charsPerToken := calibrateTokenRatio(ctx, c, model, w.CharsPerToken)

			ds, err := buildDataset(&w, charsPerToken)
			if err != nil {
				return err
			}

			charCfg := &warmup.CharacterizeConfig{
				Target:            target,
				Model:             model,
				Dataset:           ds,
				TargetConcurrency: concurrency,
				WorkloadISL:       w.ISL,
				WorkloadOSL:       w.OSL,
				CacheSalt:         w.CacheSalt,
			}

			slog.Info("Starting characterization",
				"target_concurrency", concurrency,
				"workload", w.Type,
				"isl", w.ISL,
				"osl", w.OSL)

			profile, err := warmup.RunCharacterize(ctx, charCfg)
			if err != nil {
				return fmt.Errorf("characterization failed: %w", err)
			}

			// Human-readable summary to stderr
			warmup.PrintProfileSummary(os.Stderr, profile)

			// Output profile
			out, err := json.MarshalIndent(profile, "", "  ")
			if err != nil {
				return err
			}

			if outputFile != "" {
				if err := os.WriteFile(outputFile, out, 0o644); err != nil {
					return err
				}
				slog.Info("Profile written", "path", outputFile)
			} else {
				fmt.Println(string(out))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "http://localhost:8000/v1",
		"Target endpoint base URL")
	cmd.Flags().StringVar(&model, "model", "", "Model name for requests")
	cmd.Flags().StringVar(&cfgInput, "config", "{}",
		"Workload config (JSON file or inline JSON)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 64,
		"Target concurrency to characterize up to")
	cmd.Flags().StringVar(&outputFile, "output", "",
		"Output profile JSON file (default: stdout)")

	return cmd
}
