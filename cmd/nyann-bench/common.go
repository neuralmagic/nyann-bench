package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/barrier"
	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/metrics"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// calibrateTokenRatio calibrates the chars-per-token ratio using the endpoint's tokenizer.
func calibrateTokenRatio(ctx context.Context, c *client.Client, model string, configured float64) float64 {
	if configured > 0 {
		slog.Info("Using configured chars/token", "ratio", configured)
		return configured
	}
	sample := "The quick brown fox jumps over the lazy dog. This is a sample of natural English text used to calibrate the tokenizer ratio for accurate input sequence length targeting."
	calibrated, err := c.CalibrateTokenRatio(ctx, sample, model)
	if err != nil {
		slog.Warn("Tokenizer calibration failed, using default", "error", err, "default", 4.0)
		return 4.0
	}
	slog.Info("Calibrated chars/token", "ratio", calibrated)
	return calibrated
}

// buildDataset constructs a dataset from the workload config.
func buildDataset(w *config.Workload, charsPerToken float64) (dataset.Dataset, error) {
	subISL := 0
	if w.SubsequentISL != nil {
		subISL = *w.SubsequentISL
	}

	switch w.Type {
	case "synthetic":
		ds := dataset.NewSynthetic(w.ISL, w.OSL, w.Turns, charsPerToken)
		ds.SubsequentISL = subISL
		return ds, nil
	case "faker":
		ds := dataset.NewFaker(w.ISL, w.OSL, w.Turns, charsPerToken)
		ds.SubsequentISL = subISL
		return ds, nil
	case "corpus":
		if w.CorpusPath == "" {
			return nil, fmt.Errorf("workload.corpus_path is required for corpus type")
		}
		ds, err := dataset.NewCorpus(w.CorpusPath, w.ISL, w.OSL, w.Turns, charsPerToken)
		if err != nil {
			return nil, err
		}
		ds.SubsequentISL = subISL
		return ds, nil
	case "gsm8k":
		if w.GSM8KPath == "" {
			return nil, fmt.Errorf("workload.gsm8k_path is required for gsm8k type")
		}
		w.OSL = 0
		numFewShot := 5
		if w.NumFewShot != nil {
			numFewShot = *w.NumFewShot
		}
		if numFewShot > 0 && w.GSM8KTrainPath == "" {
			return nil, fmt.Errorf("workload.gsm8k_train_path is required when num_fewshot > 0")
		}
		return dataset.NewGSM8K(w.GSM8KPath, w.GSM8KTrainPath, numFewShot)
	default:
		return nil, fmt.Errorf("unknown workload type: %s (options: synthetic, faker, corpus, gsm8k)", w.Type)
	}
}

type scenarioOpts struct {
	Target      string
	Model       string
	Scenario    *config.ScenarioConfig
	OutputDir   string
	WorkerID    int
	MetricsAddr string
	Dataset     dataset.Dataset // pre-built dataset (skips buildDataset for default workload)
}

// runScenario executes a benchmark scenario and returns the summary.
func runScenario(ctx context.Context, cancel context.CancelFunc, opts scenarioOpts) (*analysis.Summary, error) {
	target := opts.Target
	model := opts.Model
	sc := opts.Scenario

	// Wait for endpoint to be ready
	c := client.New(target)
	slog.Info("Waiting for endpoint to be ready", "target", target)
	if err := c.WaitForReady(ctx); err != nil {
		return nil, err
	}
	slog.Info("Endpoint ready")

	// Auto-detect model if not specified
	if model == "" {
		detected, err := c.DetectModel(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-detecting model (use --model to specify): %w", err)
		}
		model = detected
		slog.Info("Detected model", "model", model)
	}

	w := sc.Workload
	if w.CacheSalt != nil {
		slog.Info("Cache salt enabled", "mode", w.CacheSalt.Mode)
	}
	if w.SubsequentISL != nil {
		slog.Info("Subsequent ISL configured", "isl", w.ISL, "subsequent_isl", *w.SubsequentISL)
	}

	charsPerToken := calibrateTokenRatio(ctx, c, model, w.CharsPerToken)

	ds := opts.Dataset
	if ds == nil {
		var err error
		ds, err = buildDataset(&w, charsPerToken)
		if err != nil {
			return nil, err
		}
	}

	// Build recorder
	var rec *recorder.Recorder
	var err error
	if opts.OutputDir != "" {
		rec, err = recorder.New(opts.OutputDir, opts.WorkerID)
		if err != nil {
			return nil, fmt.Errorf("creating recorder: %w", err)
		}
	} else {
		rec = recorder.NewMemory()
	}

	// Start Prometheus metrics server
	var m *metrics.Metrics
	if opts.MetricsAddr != "" {
		reg := prometheus.NewRegistry()
		workloadName := w.Name
		if workloadName == "" {
			workloadName = w.Type
		}
		m = metrics.New(reg, workloadName, w.Type == "gsm8k")
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler(reg))
		srv := &http.Server{Addr: opts.MetricsAddr, Handler: mux}
		go func() {
			slog.Info("Metrics server listening", "addr", opts.MetricsAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Metrics server error", "error", err)
			}
		}()
	}

	// Build loadgen stages, resolving per-stage overrides.
	type resolvedStage struct {
		loadgen  loadgen.Stage
		target   string
		model    string
		workload *config.Workload
		warmup   bool
		name     string
	}

	var resolved []resolvedStage
	for i, ss := range sc.Stages {
		if ss.Barrier {
			var prevTarget, prevModel string
			var prevWorkload *config.Workload
			if i > 0 {
				prevTarget = resolved[len(resolved)-1].target
				prevModel = resolved[len(resolved)-1].model
				prevWorkload = resolved[len(resolved)-1].workload
			} else {
				prevTarget = target
				prevModel = model
			}
			resolved = append(resolved, resolvedStage{
				loadgen: loadgen.Stage{
					Barrier:      true,
					BarrierDrain: ss.BarrierDrain,
				},
				target:   prevTarget,
				model:    prevModel,
				workload: prevWorkload,
			})
			continue
		}

		effectiveTarget := target
		if ss.Target != "" {
			effectiveTarget = ss.Target
		}
		effectiveModel := model
		if ss.Model != "" {
			effectiveModel = ss.Model
		}
		var effectiveWorkload *config.Workload
		if ss.Workload != nil {
			effectiveWorkload = ss.Workload
		}

		resolved = append(resolved, resolvedStage{
			loadgen: loadgen.Stage{
				Concurrency: ss.Concurrency,
				Duration:    ss.Duration,
				Rampup:      ss.Rampup,
				MaxRequests: ss.MaxRequests,
			},
			target:   effectiveTarget,
			model:    effectiveModel,
			workload: effectiveWorkload,
			warmup:   ss.Warmup,
			name:     ss.Name,
		})
	}

	// Group consecutive stages that share the same target/workload
	// into runs that can share a single generator and stream pool.
	type stageRun struct {
		stages   []loadgen.Stage
		target   string
		model    string
		workload *config.Workload
		warmups  []bool
		names    []string
	}

	var runs []stageRun
	for _, rs := range resolved {
		canExtend := len(runs) > 0 &&
			runs[len(runs)-1].target == rs.target &&
			runs[len(runs)-1].model == rs.model &&
			workloadEqual(runs[len(runs)-1].workload, rs.workload)

		if canExtend {
			runs[len(runs)-1].stages = append(runs[len(runs)-1].stages, rs.loadgen)
			runs[len(runs)-1].warmups = append(runs[len(runs)-1].warmups, rs.warmup)
			runs[len(runs)-1].names = append(runs[len(runs)-1].names, rs.name)
		} else {
			runs = append(runs, stageRun{
				stages:   []loadgen.Stage{rs.loadgen},
				target:   rs.target,
				model:    rs.model,
				workload: rs.workload,
				warmups:  []bool{rs.warmup},
				names:    []string{rs.name},
			})
		}
	}

	totalMeasuredStages := 0
	for _, rs := range resolved {
		if !rs.warmup && !rs.loadgen.Barrier {
			totalMeasuredStages++
		}
	}

	warmupRec := recorder.NewMemory()
	var startTime time.Time
	var stageTimestamps []recorder.StageTimestamp
	globalStageIdx := 0
	measuredStageIdx := 0
	barrierIdx := 0
	var lastStageStart time.Time
	var lastConcurrency int

	for _, run := range runs {
		if ctx.Err() != nil {
			break
		}

		runTarget := run.target
		runModel := run.model
		runWorkload := &w
		if run.workload != nil {
			runWorkload = run.workload
		}

		runDS := ds
		if run.workload != nil {
			runCharsPerToken := charsPerToken
			if runWorkload.CharsPerToken > 0 {
				runCharsPerToken = runWorkload.CharsPerToken
			} else if runTarget != target {
				runC := client.New(runTarget)
				runCharsPerToken = calibrateTokenRatio(ctx, runC, runModel, runWorkload.CharsPerToken)
			}
			var err error
			runDS, err = buildDataset(runWorkload, runCharsPerToken)
			if err != nil {
				return nil, err
			}
		}

		firstStageIdx := globalStageIdx
		for firstStageIdx < len(sc.Stages) && sc.Stages[firstStageIdx].Barrier {
			firstStageIdx++
		}
		var genMode string
		var genRate float64
		var genMaxInFlight int
		if firstStageIdx < len(sc.Stages) {
			genMode = sc.Stages[firstStageIdx].Mode
			genRate = sc.Stages[firstStageIdx].Rate
			genMaxInFlight = sc.Stages[firstStageIdx].MaxInFlight
		}

		gen := &loadgen.Generator{
			Target:      runTarget,
			Model:       runModel,
			Mode:        loadgen.Mode(genMode),
			Rate:        genRate,
			MaxInFlight: genMaxInFlight,
			CacheSalt:   runWorkload.CacheSalt,
			Dataset:     runDS,
			Recorder:    rec,
			Metrics:     m,
		}

		gen.RunStages(ctx, run.stages, func(i, concurrency int) {
			isWarmup := run.warmups[i]
			stageName := run.names[i]

			if isWarmup {
				gen.SetRecorder(warmupRec)
				if stageName != "" {
					slog.Info("Warmup running", "name", stageName, "concurrency", concurrency)
				} else {
					slog.Info("Warmup running", "concurrency", concurrency)
				}
				return
			}

			gen.SetRecorder(rec)
			now := time.Now()

			if startTime.IsZero() {
				startTime = now
			} else if !lastStageStart.IsZero() {
				stageTimestamps = append(stageTimestamps, recorder.StageTimestamp{
					Stage:       measuredStageIdx - 1,
					Concurrency: lastConcurrency,
					StartTime:   recorder.TimeToFloat(lastStageStart),
					EndTime:     recorder.TimeToFloat(now),
				})
			}

			lastStageStart = now
			lastConcurrency = concurrency
			measuredStageIdx++

			if stageName != "" {
				slog.Info("Stage started",
					"name", stageName,
					"stage", fmt.Sprintf("%d/%d", measuredStageIdx, totalMeasuredStages),
					"concurrency", concurrency,
					"duration", run.stages[i].Duration)
			} else {
				slog.Info("Stage started",
					"stage", fmt.Sprintf("%d/%d", measuredStageIdx, totalMeasuredStages),
					"concurrency", concurrency,
					"duration", run.stages[i].Duration)
			}
			if m != nil {
				m.Stage.Set(float64(measuredStageIdx - 1))
			}
		}, func(i int) {
			if sc.Sync == nil || sc.Sync.Workers <= 1 {
				return
			}
			addr := fmt.Sprintf("%s:%d", sc.Sync.Addr, sc.Sync.Port)
			t, err := barrier.WaitForStart(ctx, addr, opts.WorkerID, barrierIdx, sc.Sync.Workers, sc.Sync.Timeout.Duration())
			if err != nil {
				slog.Error("Barrier failed", "error", err)
				cancel()
				return
			}
			if startTime.IsZero() {
				startTime = t
			}
			slog.Info("Barrier released", "barrier", barrierIdx, "start_time", t)
			time.Sleep(time.Until(t))
			barrierIdx++
		})

		globalStageIdx += len(run.stages)
	}

	endTime := time.Now()

	if !lastStageStart.IsZero() {
		stageTimestamps = append(stageTimestamps, recorder.StageTimestamp{
			Stage:       measuredStageIdx - 1,
			Concurrency: lastConcurrency,
			StartTime:   recorder.TimeToFloat(lastStageStart),
			EndTime:     recorder.TimeToFloat(endTime),
		})
	}

	if startTime.IsZero() {
		startTime = endTime
	}
	timestamps := &recorder.Timestamps{
		StartTime:     recorder.TimeToFloat(startTime),
		RampupEndTime: recorder.TimeToFloat(startTime),
		EndTime:       recorder.TimeToFloat(endTime),
		RampupSeconds: 0,
		TotalSeconds:  endTime.Sub(startTime).Seconds(),
		Stages:        stageTimestamps,
	}

	if opts.OutputDir != "" {
		tsPath := fmt.Sprintf("%s/timestamps_%d.json", opts.OutputDir, opts.WorkerID)
		if err := timestamps.Write(tsPath); err != nil {
			return nil, fmt.Errorf("writing timestamps: %w", err)
		}
	}

	rec.Close()
	records := rec.Records()
	if len(records) == 0 {
		return &analysis.Summary{Timestamps: timestamps}, nil
	}

	summary := analysis.Compute(records, 0, 0)
	summary.Timestamps = timestamps
	return summary, nil
}

// workloadEqual checks if two workload pointers refer to the same workload config.
func workloadEqual(a, b *config.Workload) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Type == b.Type && a.Name == b.Name &&
		a.ISL == b.ISL && a.OSL == b.OSL && a.Turns == b.Turns &&
		a.CorpusPath == b.CorpusPath && a.GSM8KPath == b.GSM8KPath
}
