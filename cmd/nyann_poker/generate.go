package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target      string
		model       string
		concurrency int
		rampup      time.Duration
		duration    time.Duration
		outputDir   string
		workerID    int
		datasetType string
		isl         int
		osl         int
		turns       int
		thinkTime   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate load against an LLM inference endpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			var ds dataset.Dataset
			switch datasetType {
			case "synthetic":
				ds = dataset.NewSynthetic(isl, osl, turns)
			default:
				return fmt.Errorf("unknown dataset type: %s", datasetType)
			}

			rec, err := recorder.New(outputDir, workerID)
			if err != nil {
				return fmt.Errorf("creating recorder: %w", err)
			}
			defer rec.Close()

			gen := &loadgen.Generator{
				Target:      target,
				Model:       model,
				Concurrency: concurrency,
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

			tsPath := fmt.Sprintf("%s/timestamps_%d.json", outputDir, workerID)
			if err := timestamps.Write(tsPath); err != nil {
				return fmt.Errorf("writing timestamps: %w", err)
			}
			fmt.Fprintf(os.Stderr, "timestamps written to %s\n", tsPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "http://localhost:8000/v1", "Target endpoint base URL")
	cmd.Flags().StringVar(&model, "model", "", "Model name for requests")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "Number of concurrent streams")
	cmd.Flags().DurationVar(&rampup, "rampup", 0, "Rampup duration to stagger stream starts")
	cmd.Flags().DurationVar(&duration, "duration", 60*time.Second, "Total benchmark duration")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "Directory for output files")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker identifier (for multi-container runs)")
	cmd.Flags().StringVar(&datasetType, "dataset", "synthetic", "Dataset type (synthetic)")
	cmd.Flags().IntVar(&isl, "isl", 128, "Input sequence length (synthetic dataset)")
	cmd.Flags().IntVar(&osl, "osl", 256, "Output sequence length (synthetic dataset)")
	cmd.Flags().IntVar(&turns, "turns", 1, "Number of turns per conversation")
	cmd.Flags().DurationVar(&thinkTime, "think-time", 0, "Think time between turns in multi-turn conversations")

	return cmd
}
