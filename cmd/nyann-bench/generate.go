package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/barrier"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/kube"
	promclient "github.com/neuralmagic/nyann-bench/pkg/prometheus"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target        string
		model         string
		cfgInput      string
		outputDir     string
		workerID      int
		workersFlag   string
		metricsAddr   string
		prometheusURL string
		deployName    string
		kubeFlags     kube.Flags
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
			// Parse config early — needed to resolve --workers auto
			sc, err := config.Parse(cfgInput)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			var measuredConditions []*config.CongestionCondition
			for _, stage := range sc.Stages {
				if !stage.Barrier && !stage.Warmup {
					measuredConditions = append(measuredConditions, stage.StopWhen)
				}
			}
			hasCongestionSweep := false
			for _, condition := range measuredConditions {
				if condition != nil {
					hasCongestionSweep = true
					break
				}
			}
			if hasCongestionSweep && (prometheusURL == "" || deployName == "") {
				return fmt.Errorf("until_congested() requires both --prometheus-url and --deploy-name")
			}

			workers, err := config.ResolveWorkers(workersFlag, sc.MaxConcurrency())
			if err != nil {
				return err
			}
			if workersFlag == "auto" {
				slog.Info("Auto-resolved workers", "workers", workers, "max_concurrency", sc.MaxConcurrency())
			}

			if kubeFlags.IsEnabled(cmd) {
				cfg, err := kubeFlags.ToConfig()
				if err != nil {
					return err
				}
				if workers > 1 {
					cfg.Workers = workers
				}
				containerArgs := kube.CollectArgs(cmd, []string{"generate"})
				containerArgs = append(containerArgs, "--metrics", ":9090")
				if cfg.Workers > 1 && !cmd.Flags().Changed("workers") {
					containerArgs = append(containerArgs, "--workers", strconv.Itoa(cfg.Workers))
				}
				return kube.Deploy(cfg, "generate", containerArgs)
			}

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

			sc.Workers = workers
			sc.WorkerID = workerID

			// Configure barrier sync for multi-worker runs
			if workers > 1 {
				syncCfg := &config.SyncConfig{
					Workers: workers,
					Timeout: config.Duration(10 * time.Minute),
					Port:    8080,
				}
				if addr, ok := os.LookupEnv("BARRIER_ADDR"); ok {
					syncCfg.Addr = addr
				} else {
					syncCfg.Addr = "localhost"
				}
				sc.Sync = syncCfg
				sc.InsertImplicitBarrier()

				if workerID == 0 {
					srv := barrier.NewServer(workers, syncCfg.Port)
					go srv.ListenAndServe(ctx)
				}

				slog.Info("Sync enabled", "workers", workers, "addr", syncCfg.Addr, "port", syncCfg.Port)
			}

			// CLI flags override config-level target/model
			if sc.Target != "" && target == "http://localhost:8000/v1" {
				target = sc.Target
			}
			if sc.Model != "" && model == "" {
				model = sc.Model
			}

			var promClient *promclient.Client
			if prometheusURL != "" && deployName != "" {
				promClient = promclient.NewClient(prometheusURL)
				if strings.Contains(deployName, "-decode") {
					prefillDeploy := strings.Replace(deployName, "-decode", "-prefill", 1)
					slog.Info("Prometheus scrape targets",
						"url", prometheusURL,
						"mode", "pd",
						"decode_pods", deployName+".*",
						"prefill_pods", prefillDeploy+".*")
				} else {
					slog.Info("Prometheus scrape targets",
						"url", prometheusURL,
						"mode", "aggregate",
						"pods", deployName+".*")
				}
			}

			headerPrinted := false
			var collected []*analysis.ServerMetrics

			summary, err := runScenario(ctx, cancel, scenarioOpts{
				Target:      target,
				Model:       model,
				Scenario:    sc,
				OutputDir:   outputDir,
				WorkerID:    workerID,
				MetricsAddr: metricsAddr,
				OnStageComplete: func(ts recorder.StageTimestamp, records []recorder.Record) bool {
					stages := analysis.ComputePerStage(records, []recorder.StageTimestamp{ts})
					if len(stages) == 0 {
						return true
					}
					stage := stages[0]

					if promClient != nil {
						server := analysis.QueryStageServerMetrics(promClient, ts, deployName)
						stage.Server = server
						collected = append(collected, server)
					} else {
						collected = append(collected, nil)
					}

					if !headerPrinted {
						fmt.Fprint(os.Stderr, analysis.FormatStageHeader(stage.Server))
						headerPrinted = true
					}
					fmt.Fprint(os.Stderr, analysis.FormatStageRow(stage))

					if ts.Stage >= len(measuredConditions) || measuredConditions[ts.Stage] == nil {
						return true
					}
					result, err := analysis.CheckStageCongestion(promClient, ts, deployName, *measuredConditions[ts.Stage])
					if err != nil {
						slog.Warn("Some congestion metrics could not be queried", "error", err)
					}
					for _, role := range result.Roles {
						slog.Info("Congestion signals",
							"role", role.Role,
							"waiting_max", role.WaitingMax,
							"ttft_p99", time.Duration(role.TTFTP99*float64(time.Second)),
							"kv_usage_max", role.KVUsageMax,
							"preemptions", role.Preemptions)
					}
					if result.Congested {
						slog.Warn("Congestion reached; stopping stage sweep", "reasons", strings.Join(result.Reasons, "; "))
						return false
					}
					return true
				},
			})
			if err != nil {
				return err
			}

			// Requery Prometheus for all stages now that the benchmark is done.
			if promClient != nil && summary.Timestamps != nil {
				for i, ts := range summary.Timestamps.Stages {
					if i < len(summary.Stages) {
						summary.Stages[i].Server = analysis.QueryStageServerMetrics(promClient, ts, deployName)
					}
				}
			}

			if summary.TotalRequests > 0 {
				fmt.Fprint(os.Stderr, "\n")
				if len(summary.Stages) > 0 {
					var serverRef *analysis.ServerMetrics
					for _, s := range summary.Stages {
						if s.Server.HasData() {
							serverRef = s.Server
							break
						}
					}
					fmt.Fprint(os.Stderr, analysis.FormatStageHeader(serverRef))
					for _, s := range summary.Stages {
						fmt.Fprint(os.Stderr, analysis.FormatStageRow(s))
					}
					fmt.Fprint(os.Stderr, "\n")
				}
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
	cmd.Flags().StringVar(&workersFlag, "workers", "1", `Number of workers: integer or "auto" (auto = ceil(max_concurrency/1024))`)
	cmd.Flags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics listen address (e.g. :9090)")
	cmd.Flags().StringVar(&prometheusURL, "prometheus-url", "", "Prometheus server URL for querying server-side vLLM metrics (e.g. http://prometheus:9090)")
	cmd.Flags().StringVar(&deployName, "deploy-name", "", "Deployment name prefix for Prometheus pod label filtering (e.g. my-deploy)")

	kube.RegisterFlags(cmd, &kubeFlags)

	return cmd
}
