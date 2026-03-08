package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/neuralmagic/nyann_poker/pkg/analysis"
	"github.com/spf13/cobra"
)

func analyzeCmd() *cobra.Command {
	var (
		dir          string
		warmupBuffer float64
		jsonOutput   bool
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze benchmark results from client-side JSONL recordings",
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := analysis.LoadRecords(dir)
			if err != nil {
				return err
			}

			// Determine measurement window from timestamps
			var startTime, endTime float64
			tsStart, tsEnd, err := analysis.LoadTimestamps(dir)
			if err == nil {
				startTime = tsStart + warmupBuffer
				endTime = tsEnd
				slog.Info("Measurement window",
					"window_s", endTime-startTime,
					"after_rampup_s", tsEnd-tsStart,
					"warmup_buffer_s", warmupBuffer)
			} else {
				slog.Info("No timestamps found, using all records")
			}

			summary := analysis.Compute(records, startTime, endTime)

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}

			fmt.Print(analysis.FormatSummary(summary))
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "Directory containing requests_*.jsonl and timestamps_*.json files")
	cmd.Flags().Float64Var(&warmupBuffer, "warmup-buffer", 0, "Additional seconds to skip after rampup before measuring")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")

	return cmd
}
