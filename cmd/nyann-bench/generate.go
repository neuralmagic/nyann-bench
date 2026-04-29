package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/barrier"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target      string
		model       string
		cfgInput    string
		outputDir   string
		workerID    int
		metricsAddr string
		syncFlag    string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate load against an LLM inference endpoint",
		Long: `Generate load against an LLM inference endpoint.

Configure the workload via --config (JSON file, inline JSON, or Starlark .star file):

  nyann-bench generate --target http://localhost:8000/v1 --model my-model \
    --config '{"load":{"mode":"concurrent","concurrency":10,"duration":"60s"},"workload":{"type":"faker","isl":128,"osl":256}}'

  nyann-bench generate --target http://localhost:8000/v1 --config benchmark.json

  nyann-bench generate --config scenario.star

Starlark (.star) files provide full programmability — loops, functions,
conditionals, and per-stage workload/target overrides:

  scenario(
      stages = [stage("2m", concurrency=c) for c in range(10, 101, 10)],
      workload = workload("faker", isl=512, osl=1024),
  )

Load modes:
  concurrent  Fixed number of streams, each fires next request on completion (default)
  constant    Requests arrive at a fixed rate (evenly spaced)
  poisson     Requests arrive at a target rate with exponential inter-arrival times

Workload types:
  synthetic   Random word padding
  faker       Diverse generated prose (gofakeit)
  corpus      Sliding window over real text files
  gsm8k       GSM8K math problems with streaming eval`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Auto-detect worker ID from K8s (LWS or indexed Job)
			if workerID == 0 {
				if idx, ok := os.LookupEnv("LWS_WORKER_INDEX"); ok {
					if v, err := strconv.Atoi(idx); err == nil {
						workerID = v
					}
				} else if idx, ok := os.LookupEnv("JOB_COMPLETION_INDEX"); ok {
					if v, err := strconv.Atoi(idx); err == nil {
						workerID = v
					}
				}
			}

			// Parse config
			sc, err := config.Parse(cfgInput)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}

			// Parse --sync flag and configure barrier
			if syncFlag != "" {
				syncCfg, err := config.ParseSyncFlag(syncFlag)
				if err != nil {
					return err
				}
				if syncCfg.Addr == "" {
					if addr, ok := os.LookupEnv("LWS_LEADER_ADDRESS"); ok {
						syncCfg.Addr = addr
					}
				}
				if workerID == 0 && syncCfg.Addr == "" {
					syncCfg.Addr = "localhost"
				}
				sc.Sync = syncCfg

				sc.InsertImplicitBarrier()

				if syncCfg.Workers > 1 && workerID == 0 {
					srv := barrier.NewServer(syncCfg.Workers, syncCfg.Port)
					go srv.ListenAndServe(ctx)
				}

				slog.Info("Sync enabled", "workers", syncCfg.Workers, "addr", syncCfg.Addr, "port", syncCfg.Port)
			}

			// CLI flags override config-level target/model
			if sc.Target != "" && target == "http://localhost:8000/v1" {
				target = sc.Target
			}
			if sc.Model != "" && model == "" {
				model = sc.Model
			}

			summary, err := runScenario(ctx, cancel, scenarioOpts{
				Target:      target,
				Model:       model,
				Scenario:    sc,
				OutputDir:   outputDir,
				WorkerID:    workerID,
				MetricsAddr: metricsAddr,
			})
			if err != nil {
				return err
			}

			if summary.TotalRequests > 0 {
				fmt.Fprint(os.Stderr, "\n")
				fmt.Fprint(os.Stderr, analysis.FormatSummary(summary))

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
	cmd.Flags().StringVar(&cfgInput, "config", "{}", "Workload config (JSON file, inline JSON, or .star file)")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for JSONL + timestamp files (omit for stdout-only)")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker identifier (for multi-container runs)")
	cmd.Flags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics listen address (e.g. :9090)")
	cmd.Flags().StringVar(&syncFlag, "sync", "", `Barrier sync config JSON (e.g. '{"workers":4,"timeout":"10m"}')`)

	return cmd
}
