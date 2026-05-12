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
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/kube"
	"github.com/spf13/cobra"
)

func evalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run evaluation benchmarks",
		Long:  "Run standardized evaluation benchmarks against an LLM endpoint.",
	}
	cmd.AddCommand(evalGSM8KCmd())
	cmd.AddCommand(evalGPQACmd())
	return cmd
}

func evalGSM8KCmd() *cobra.Command {
	var (
		target         string
		model          string
		concurrency    int
		gsm8kPath      string
		gsm8kTrainPath string
		numFewShot     int
		timeout        string
		outputDir      string
		metricsAddr    string
		workerID       int
		workers        int
		kubeFlags      kube.Flags
	)

	cmd := &cobra.Command{
		Use:   "gsm8k",
		Short: "Evaluate GSM8K math accuracy under load",
		Long: `Run the GSM8K evaluation benchmark against an LLM endpoint.

Sends all GSM8K test problems with few-shot prompting, evaluates
correctness of model responses, and reports accuracy alongside latency metrics.

For multi-worker scale-out (e.g., Indexed Job), use --workers and
--worker-id to partition the dataset across workers. Each worker runs a
disjoint slice, and --worker-id auto-detects from JOB_COMPLETION_INDEX.

Example:
  nyann-bench eval gsm8k --target http://localhost:8000/v1 --model llama-70b \
    --gsm8k-path data/gsm8k_test.jsonl --gsm8k-train-path data/gsm8k_train.jsonl

  # Scale-out: 4 workers, each runs ~330 items
  nyann-bench eval gsm8k --target http://localhost:8000/v1 --model llama-70b \
    --gsm8k-path data/gsm8k_test.jsonl --gsm8k-train-path data/gsm8k_train.jsonl \
    --workers 4`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if kubeFlags.IsEnabled(cmd) {
				cfg, err := kubeFlags.ToConfig()
				if err != nil {
					return err
				}
				if workers > 1 {
					cfg.Workers = workers
				}
				containerArgs := kube.CollectArgs(cmd, []string{"eval", "gsm8k"})
				containerArgs = append(containerArgs, "--metrics", ":9090")
				return kube.Deploy(cfg, "eval-gsm8k", containerArgs)
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			timeoutDur, err := time.ParseDuration(timeout)
			if err != nil {
				return fmt.Errorf("invalid timeout %q: %w", timeout, err)
			}

			if gsm8kPath == "" {
				return fmt.Errorf("--gsm8k-path is required (path to GSM8K test JSONL)")
			}
			if numFewShot > 0 && gsm8kTrainPath == "" {
				return fmt.Errorf("--gsm8k-train-path is required when --num-fewshot > 0")
			}

			// Auto-detect worker ID from K8s indexed Job
			if workerID == 0 {
				if idx, ok := os.LookupEnv("JOB_COMPLETION_INDEX"); ok {
					if v, err := strconv.Atoi(idx); err == nil {
						workerID = v
						slog.Info("Auto-detected worker ID", "env", "JOB_COMPLETION_INDEX", "worker_id", workerID)
					}
				}
			}

			if workers > 1 && workerID >= workers {
				return fmt.Errorf("--worker-id %d must be < --workers %d", workerID, workers)
			}

			// Build dataset and partition for this worker
			gsm8kDS, err := dataset.NewGSM8K(gsm8kPath, gsm8kTrainPath, numFewShot)
			if err != nil {
				return fmt.Errorf("loading GSM8K dataset: %w", err)
			}
			totalItems := gsm8kDS.Len()
			if workers > 1 {
				gsm8kDS.Partition(workerID, workers)
			}
			partitionItems := gsm8kDS.Len()

			slog.Info("GSM8K eval configured",
				"total_items", totalItems,
				"partition_items", partitionItems,
				"worker_id", workerID,
				"workers", workers,
				"concurrency", concurrency,
				"timeout", timeout,
				"num_fewshot", numFewShot)

			sc := &config.ScenarioConfig{
				Target: target,
				Model:  model,
				Workload: config.Workload{
					Type:           "gsm8k",
					GSM8KPath:      gsm8kPath,
					GSM8KTrainPath: gsm8kTrainPath,
					NumFewShot:     &numFewShot,
				},
				Stages: []config.ScenarioStage{{
					Name:        "gsm8k-eval",
					Duration:    timeoutDur,
					Mode:        "concurrent",
					Concurrency: concurrency,
					MaxRequests: partitionItems,
				}},
				Workers:  workers,
				WorkerID: workerID,
			}

			summary, err := runScenario(ctx, cancel, scenarioOpts{
				Target:      target,
				Model:       model,
				Scenario:    sc,
				OutputDir:   outputDir,
				WorkerID:    workerID,
				MetricsAddr: metricsAddr,
				Dataset:     gsm8kDS,
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

	cmd.Flags().StringVar(&target, "target", "", "Target endpoint base URL (required)")
	cmd.Flags().StringVar(&model, "model", "", "Model name (auto-detected if omitted)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 64, "Number of concurrent streams")
	cmd.Flags().StringVar(&gsm8kPath, "gsm8k-path", "", "Path to GSM8K test JSONL file (required)")
	cmd.Flags().StringVar(&gsm8kTrainPath, "gsm8k-train-path", "", "Path to GSM8K train JSONL (for few-shot examples)")
	cmd.Flags().IntVar(&numFewShot, "num-fewshot", 5, "Number of few-shot examples (0 for zero-shot)")
	cmd.Flags().StringVar(&timeout, "timeout", "30m", "Hard time cap for the evaluation")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for JSONL + timestamp output files")
	cmd.Flags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics listen address (e.g. :9090)")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker index for dataset partitioning (auto-detected from JOB_COMPLETION_INDEX)")
	cmd.Flags().IntVar(&workers, "workers", 1, "Total number of workers (partitions dataset and divides concurrency when > 1)")

	kube.RegisterFlags(cmd, &kubeFlags)

	cmd.MarkFlagRequired("target")
	cmd.MarkFlagRequired("gsm8k-path")

	return cmd
}

func evalGPQACmd() *cobra.Command {
	var (
		target      string
		model       string
		concurrency int
		gpqaPath    string
		timeout     string
		outputDir   string
		metricsAddr string
		workerID    int
		workers     int
		maxTokens   int
		kubeFlags   kube.Flags
	)

	cmd := &cobra.Command{
		Use:   "gpqa",
		Short: "Evaluate GPQA multiple-choice accuracy under load",
		Long: `Run the GPQA (Graduate-Level Google-Proof Q&A) evaluation benchmark.

Sends all GPQA questions with chain-of-thought prompting, extracts
multiple-choice answers, and reports accuracy alongside latency metrics.

Supports two data formats:
  - Idavidrein/gpqa: separate choice fields (gated, requires HF auth)
  - fingertap/GPQA-Diamond: choices inline in question (public)

For multi-worker scale-out, use --workers and --worker-id.

Example:
  nyann-bench eval gpqa --target http://localhost:8000/v1 --model llama-70b \
    --gpqa-path data/gpqa_diamond.jsonl

  # Scale-out: 4 workers
  nyann-bench eval gpqa --target http://localhost:8000/v1 --model llama-70b \
    --gpqa-path data/gpqa_diamond.jsonl --workers 4`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if kubeFlags.IsEnabled(cmd) {
				cfg, err := kubeFlags.ToConfig()
				if err != nil {
					return err
				}
				if workers > 1 {
					cfg.Workers = workers
				}
				containerArgs := kube.CollectArgs(cmd, []string{"eval", "gpqa"})
				containerArgs = append(containerArgs, "--metrics", ":9090")
				return kube.Deploy(cfg, "eval-gpqa", containerArgs)
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			timeoutDur, err := time.ParseDuration(timeout)
			if err != nil {
				return fmt.Errorf("invalid timeout %q: %w", timeout, err)
			}

			if gpqaPath == "" {
				return fmt.Errorf("--gpqa-path is required (path to GPQA JSONL)")
			}

			if workerID == 0 {
				if idx, ok := os.LookupEnv("JOB_COMPLETION_INDEX"); ok {
					if v, err := strconv.Atoi(idx); err == nil {
						workerID = v
						slog.Info("Auto-detected worker ID", "env", "JOB_COMPLETION_INDEX", "worker_id", workerID)
					}
				}
			}

			if workers > 1 && workerID >= workers {
				return fmt.Errorf("--worker-id %d must be < --workers %d", workerID, workers)
			}

			gpqaDS, err := dataset.NewGPQA(gpqaPath, maxTokens)
			if err != nil {
				return fmt.Errorf("loading GPQA dataset: %w", err)
			}
			totalItems := gpqaDS.Len()
			if workers > 1 {
				gpqaDS.Partition(workerID, workers)
			}
			partitionItems := gpqaDS.Len()

			slog.Info("GPQA eval configured",
				"total_items", totalItems,
				"partition_items", partitionItems,
				"worker_id", workerID,
				"workers", workers,
				"concurrency", concurrency,
				"timeout", timeout)

			sc := &config.ScenarioConfig{
				Target: target,
				Model:  model,
				Workload: config.Workload{
					Type:     "gpqa",
					GPQAPath: gpqaPath,
				},
				Stages: []config.ScenarioStage{{
					Name:        "gpqa-eval",
					Duration:    timeoutDur,
					Mode:        "concurrent",
					Concurrency: concurrency,
					MaxRequests: partitionItems,
				}},
				Workers:  workers,
				WorkerID: workerID,
			}

			summary, err := runScenario(ctx, cancel, scenarioOpts{
				Target:      target,
				Model:       model,
				Scenario:    sc,
				OutputDir:   outputDir,
				WorkerID:    workerID,
				MetricsAddr: metricsAddr,
				Dataset:     gpqaDS,
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

	cmd.Flags().StringVar(&target, "target", "", "Target endpoint base URL (required)")
	cmd.Flags().StringVar(&model, "model", "", "Model name (auto-detected if omitted)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 64, "Number of concurrent streams")
	cmd.Flags().StringVar(&gpqaPath, "gpqa-path", "", "Path to GPQA JSONL file (required)")
	cmd.Flags().StringVar(&timeout, "timeout", "30m", "Hard time cap for the evaluation")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for JSONL + timestamp output files")
	cmd.Flags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics listen address (e.g. :9090)")
	cmd.Flags().IntVar(&workerID, "worker-id", 0, "Worker index for dataset partitioning (auto-detected from JOB_COMPLETION_INDEX)")
	cmd.Flags().IntVar(&workers, "workers", 1, "Total number of workers (partitions dataset and divides concurrency when > 1)")
	cmd.Flags().IntVar(&maxTokens, "max-tokens", 0, "Max output tokens per request (0 = default 16384, increase for reasoning models)")

	kube.RegisterFlags(cmd, &kubeFlags)

	cmd.MarkFlagRequired("target")
	cmd.MarkFlagRequired("gpqa-path")

	return cmd
}
