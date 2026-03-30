package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neuralmagic/nyann_poker/pkg/analysis"
	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/metrics"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
	"github.com/neuralmagic/nyann_poker/pkg/warmup"
	"github.com/spf13/cobra"
)

func generateCmd() *cobra.Command {
	var (
		target     string
		model      string
		cfgInput   string
		outputDir  string
		workerID   int
		metricsAddr string
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
  corpus      Sliding window over real text files (--corpus-path in config)
  gsm8k       GSM8K math problems with streaming eval (--gsm8k-path in config)`,
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

			// Wait for endpoint to be ready
			c := client.New(target)
			slog.Info("Waiting for endpoint to be ready", "target", target)
			if err := c.WaitForReady(ctx); err != nil {
				return err
			}
			slog.Info("Endpoint ready")

			// Auto-detect model if not specified
			if model == "" {
				detected, err := c.DetectModel(ctx)
				if err != nil {
					return fmt.Errorf("auto-detecting model (use --model to specify): %w", err)
				}
				model = detected
				slog.Info("Detected model", "model", model)
			}

			// Parse config
			cfg, err := config.Parse(cfgInput)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}

			w := cfg.Workload
			if w.CacheSalt != nil {
				slog.Info("Cache salt enabled", "mode", w.CacheSalt.Mode)
			}

			charsPerToken := calibrateTokenRatio(ctx, c, model, w.CharsPerToken)

			ds, err := buildDataset(&w, charsPerToken)
			if err != nil {
				return err
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

			// Start Prometheus metrics server
			var m *metrics.Metrics
			if metricsAddr != "" {
				reg := prometheus.NewRegistry()
				workloadName := w.Name
				if workloadName == "" {
					workloadName = w.Type
				}
				m = metrics.New(reg, workloadName, w.Type == "gsm8k")
				mux := http.NewServeMux()
				mux.Handle("/metrics", metrics.Handler(reg))
				srv := &http.Server{Addr: metricsAddr, Handler: mux}
				go func() {
					slog.Info("Metrics server listening", "addr", metricsAddr)
					if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						slog.Error("Metrics server error", "error", err)
					}
				}()
			}

			// Build combined stages: warmup (if any) + main stages
			cfgStages := cfg.EffectiveStages()
			warmupStages := 0

			var allStages []loadgen.Stage
			if cfg.Warmup != nil {
				warmupCfg := &warmup.Config{
					Duration:    cfg.Warmup.Duration.Duration(),
					Concurrency: cfgStages[0].Concurrency,
					Stagger:     cfg.Warmup.Stagger,
				}
				ws, err := warmup.Stage(warmupCfg)
				if err != nil {
					return fmt.Errorf("warmup: %w", err)
				}
				allStages = append(allStages, ws)
				warmupStages = 1
			}
			for _, s := range cfgStages {
				allStages = append(allStages, loadgen.Stage{
					Concurrency: s.Concurrency,
					Duration:    s.Duration.Duration(),
				})
			}

			// Start with a discard recorder; swap to the real one after warmup
			warmupRec := recorder.NewMemory()
			gen := &loadgen.Generator{
				Target:      target,
				Model:       model,
				Mode:        loadgen.Mode(cfg.Load.Mode),
				Rate:        cfg.Load.Rate,
				MaxInFlight: cfg.Load.MaxInFlight,
				Rampup:      cfg.Load.Rampup.Duration(),
				CacheSalt:   w.CacheSalt,
				Dataset:     ds,
				Recorder:    warmupRec,
				Metrics:     m,
			}
			if cfg.Warmup == nil {
				gen.Recorder = rec
			}

			var startTime time.Time
			gen.RunStages(ctx, allStages, func(i, concurrency int) {
				if i < warmupStages {
					slog.Info("Warmup running", "concurrency", concurrency)
					return
				}
				mainIdx := i - warmupStages
				if mainIdx == 0 {
					// Transition from warmup to main: swap recorder
					if cfg.Warmup != nil {
						gen.SetRecorder(rec)
						slog.Info("Warmup complete",
							"requests", len(warmupRec.Records()))
					}
					startTime = time.Now()
				}
				slog.Info("Stage started",
					"stage", fmt.Sprintf("%d/%d", mainIdx+1, len(cfgStages)),
					"concurrency", concurrency,
					"duration", allStages[i].Duration)
				if m != nil {
					m.Stage.Set(float64(mainIdx))
				}
			})

			endTime := time.Now()
			timestamps := &recorder.Timestamps{
				StartTime:     recorder.TimeToFloat(startTime),
				RampupEndTime: recorder.TimeToFloat(startTime.Add(cfg.Load.Rampup.Duration())),
				EndTime:       recorder.TimeToFloat(endTime),
				RampupSeconds: cfg.Load.Rampup.Duration().Seconds(),
				TotalSeconds:  endTime.Sub(startTime).Seconds(),
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
	cmd.Flags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics listen address (e.g. :9090)")

	return cmd
}
