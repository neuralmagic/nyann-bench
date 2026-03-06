package main

import (
	"fmt"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/mockserver"
	"github.com/spf13/cobra"
)

func mockServerCmd() *cobra.Command {
	var (
		addr         string
		ttft         time.Duration
		itl          time.Duration
		outputTokens int
		model        string
	)

	cmd := &cobra.Command{
		Use:   "mock-server",
		Short: "Run a mock OpenAI-compatible inference server for testing",
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := &mockserver.Server{
				Addr:         addr,
				TTFT:         ttft,
				ITL:          itl,
				OutputTokens: outputTokens,
				Model:        model,
			}
			fmt.Fprintf(cmd.OutOrStderr(), "Starting mock server on %s (ttft=%s, itl=%s, tokens=%d)\n",
				addr, ttft, itl, outputTokens)
			return srv.ListenAndServe()
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8000", "Listen address")
	cmd.Flags().DurationVar(&ttft, "ttft", 50*time.Millisecond, "Simulated time to first token")
	cmd.Flags().DurationVar(&itl, "itl", 10*time.Millisecond, "Simulated inter-token latency")
	cmd.Flags().IntVar(&outputTokens, "output-tokens", 128, "Number of output tokens per response")
	cmd.Flags().StringVar(&model, "model", "mock-model", "Model name to report")

	return cmd
}
