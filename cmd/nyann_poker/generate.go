package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/neuralmagic/nyann_poker/pkg/analysis"
	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target    string
		model     string
		cfgInput  string
		outputDir string
		workerID  int
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate load against an LLM inference endpoint",
		Long: `Generate load against an LLM inference endpoint.

Configure the workload via --config (JSON file or inline JSON):

  nyann_poker generate --target http://localhost:8000/v1 --model my-model \
    --config '{"load":{"mode":"concurrent","concurrency":10,"duration":"60s"},"workload":{"type":"faker","isl":128,"osl":256}}'

  nyann_poker generate --target http://localhost:8000/v1 --model my-model \
    --config benchmark.json

Load modes:
  concurrent  Fixed number of streams, each fires next request on completion (default)
  constant    Requests arrive at a fixed rate (evenly spaced)
  poisson     Requests arrive at a target rate with exponential inter-arrival times

Workload types:
  synthetic   Random word padding
  faker       Diverse generated prose (gofakeit)
  corpus      Sliding window over real text files (--corpus-path in config)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Auto-detect worker ID from K8s indexed Job
			if workerID == 0 {
				if idx, ok := os.LookupEnv("JOB_COMPLETION_INDEX"); ok {
					if v, err := strconv.Atoi(idx); err == nil {
						workerID = v
					}
				}
			}

			// Parse config
			cfg, err := config.Parse(cfgInput)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}

			// Build dataset
			var ds dataset.Dataset
			w := cfg.Workload
			switch w.Type {
			case "synthetic":
				ds = dataset.NewSynthetic(w.ISL, w.OSL, w.Turns)
			case "faker":
				ds = dataset.NewFaker(w.ISL, w.OSL, w.Turns)
			case "corpus":
				if w.CorpusPath == "" {
					return fmt.Errorf("workload.corpus_path is required for corpus type")
				}
				ds, err = dataset.NewCorpus(w.CorpusPath, w.ISL, w.OSL, w.Turns)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown workload type: %s (options: synthetic, faker, corpus)", w.Type)
			}

			// Build recorder
			var rec *recorder.Recorder
			if outputDir != "" {
				rec, err = recorder.New(outputDir, workerID)
				if err != nil {
					return fmt.Errorf("creating recorder: %w", err)
				}
			} else {
				rec = recorder.NewMemory()
			}

			// Build and run generator
			gen := &loadgen.Generator{
				Target:      target,
				Model:       model,
				Mode:        loadgen.Mode(cfg.Load.Mode),
				Concurrency: cfg.Load.Concurrency,
				Rate:        cfg.Load.Rate,
				MaxInFlight: cfg.Load.MaxInFlight,
				Rampup:      cfg.Load.Rampup.Duration(),
				Duration:    cfg.Load.Duration.Duration(),
				Dataset:     ds,
				Recorder:    rec,
			}

			timestamps, err := gen.Run(ctx)
			if err != nil {
				return err
			}

			// Write files to disk if output-dir is set
			if outputDir != "" {
				tsPath := fmt.Sprintf("%s/timestamps_%d.json", outputDir, workerID)
				if err := timestamps.Write(tsPath); err != nil {
					return fmt.Errorf("writing timestamps: %w", err)
				}
			}

			// Compute and print summary
			rec.Close()
			records := rec.Records()
			if len(records) > 0 {
				summary := analysis.Compute(records, 0, 0)
				summary.Timestamps = timestamps

				// Human-readable to stderr
				fmt.Fprint(os.Stderr, "\n")
				fmt.Fprint(os.Stderr, analysis.FormatSummary(summary))

				// Machine-readable JSON to stdout
				jsonOut, err := json.MarshalIndent(summary, "", "  ")
				if err != nil {
					return fmt.Errorf("marshalling summary: %w", err)
				}
				fmt.Println(string(jsonOut))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "http://localhost:8000/v1", "Target endpoint base URL")
	cmd.Flags().StringVar(&model, "model", "", "Model name for requests")
	cmd.Flags().StringVar(&cfgInput, "config", "{}", "Workload config (JSON file path or inline JSON)")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for JSONL + timestamp files (omit for stdout-only)")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker identifier (for multi-container runs)")

	return cmd
}
