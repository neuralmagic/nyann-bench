package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/analysis"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target      string
		model       string
		mode        string
		concurrency int
		rate        float64
		maxInFlight int
		rampup      time.Duration
		duration    time.Duration
		outputDir   string
		workerID    int
		datasetType string
		isl         int
		osl         int
		turns       int
		thinkTime   time.Duration
		corpusPath  string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate load against an LLM inference endpoint",
		Long: `Generate load against an LLM inference endpoint.

Modes:
  concurrent  Fixed number of streams, each fires next request on completion (default)
  constant    Requests arrive at a fixed rate (evenly spaced)
  poisson     Requests arrive at a target rate with exponential inter-arrival times`,
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

			var ds dataset.Dataset
			switch datasetType {
			case "synthetic":
				ds = dataset.NewSynthetic(isl, osl, turns)
			case "faker":
				ds = dataset.NewFaker(isl, osl, turns)
			case "corpus":
				if corpusPath == "" {
					return fmt.Errorf("--corpus-path is required when using corpus dataset")
				}
				var err error
				ds, err = dataset.NewCorpus(corpusPath, isl, osl, turns)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown dataset type: %s (options: synthetic, faker, corpus)", datasetType)
			}

			var rec *recorder.Recorder
			if outputDir != "" {
				var err error
				rec, err = recorder.New(outputDir, workerID)
				if err != nil {
					return fmt.Errorf("creating recorder: %w", err)
				}
			} else {
				rec = recorder.NewMemory()
			}

			gen := &loadgen.Generator{
				Target:      target,
				Model:       model,
				Mode:        loadgen.Mode(mode),
				Concurrency: concurrency,
				Rate:        rate,
				MaxInFlight: maxInFlight,
				Rampup:      rampup,
				Duration:    duration,
				Dataset:     ds,
				Recorder:    rec,
				ThinkTime:   thinkTime,
			}

			timestamps, err := gen.Run(ctx)
			if err != nil {
				return err
			}

			// Write timestamps and JSONL to disk if output-dir is set
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
	cmd.Flags().StringVar(&mode, "mode", "concurrent", "Load generation mode: concurrent, constant, poisson")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "Number of concurrent streams (concurrent mode)")
	cmd.Flags().Float64Var(&rate, "rate", 10.0, "Request rate in req/s (constant/poisson mode)")
	cmd.Flags().IntVar(&maxInFlight, "max-inflight", 0, "Max concurrent requests (constant/poisson mode, 0=unlimited)")
	cmd.Flags().DurationVar(&rampup, "rampup", 0, "Rampup duration (stagger streams or ramp rate)")
	cmd.Flags().DurationVar(&duration, "duration", 60*time.Second, "Total benchmark duration")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for JSONL + timestamp files (omit for stdout-only)")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker identifier (for multi-container runs)")
	cmd.Flags().StringVar(&datasetType, "dataset", "synthetic", "Dataset type (synthetic, faker, corpus)")
	cmd.Flags().StringVar(&corpusPath, "corpus-path", "", "Path to text file or directory (corpus dataset)")
	cmd.Flags().IntVar(&isl, "isl", 128, "Input sequence length (synthetic dataset)")
	cmd.Flags().IntVar(&osl, "osl", 256, "Output sequence length (synthetic dataset)")
	cmd.Flags().IntVar(&turns, "turns", 1, "Number of turns per conversation")
	cmd.Flags().DurationVar(&thinkTime, "think-time", 0, "Think time between turns in multi-turn conversations")

	return cmd
}
