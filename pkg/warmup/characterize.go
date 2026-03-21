package warmup

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
)

// CharacterizeConfig holds parameters for RunCharacterize.
type CharacterizeConfig struct {
	Target            string
	Model             string
	Dataset           dataset.Dataset
	TargetConcurrency int
	WorkloadISL       int
	WorkloadOSL       int
	CacheSalt         *config.CacheSalt

	// Tuning (zero values use defaults)
	MaxKernelRequests  int     // cap on kernel warmup probes (default: 20)
	StabilityThreshold float64 // TTFT change threshold (default: 0.10)
	StabilityWindow    int     // consecutive stable readings (default: 2)
	RequestsPerStream  int     // requests per stream at each level (default: 5)
	DiscardPerLevel    int     // discard first N records per level (default: 2)
}

func (c *CharacterizeConfig) defaults() {
	if c.MaxKernelRequests <= 0 {
		c.MaxKernelRequests = 20
	}
	if c.StabilityThreshold <= 0 {
		c.StabilityThreshold = 0.10
	}
	if c.StabilityWindow <= 0 {
		c.StabilityWindow = 2
	}
	if c.RequestsPerStream <= 0 {
		c.RequestsPerStream = 5
	}
	if c.DiscardPerLevel <= 0 {
		c.DiscardPerLevel = 2
	}
}

// RunCharacterize runs a characterization sweep and returns a fitted profile.
//
// Phase 1: Sequential kernel warmup at C=1 (detects when TTFT stabilizes).
// Phase 2: D-optimal measurement at {1, target/2, target}.
// Phase 3: Fit TPOT(C) = a + b*C + c*C² and compute derived metrics.
func RunCharacterize(ctx context.Context, cfg *CharacterizeConfig) (*Profile, error) {
	cfg.defaults()

	cl := client.New(cfg.Target)

	// Phase 1: Kernel warmup detection
	slog.Info("Phase 1: kernel warmup detection")
	kernelCount, kernelResults, err := detectKernelWarmup(ctx, cl, cfg)
	if err != nil {
		return nil, fmt.Errorf("kernel warmup: %w", err)
	}
	slog.Info("Kernel warmup complete",
		"requests", kernelCount,
		"final_ttft_ms", fmt.Sprintf("%.1f", kernelResults[len(kernelResults)-1].ttft))

	// Phase 2: Measurement at D-optimal points
	// C=1 data comes from post-warmup kernel probes
	levels := measureLevels(ctx, cfg, cl, kernelResults)

	if len(levels) == 0 {
		return nil, fmt.Errorf("no measurement levels collected")
	}

	// Phase 3: Fit model
	xs := make([]float64, len(levels))
	ys := make([]float64, len(levels))
	for i, l := range levels {
		xs[i] = float64(l.Concurrency)
		ys[i] = l.TPOTMs.Mean
	}

	var model QuadModel
	if len(levels) >= 3 {
		model = FitQuadratic(xs, ys)
	} else {
		model = FitLinear(xs, ys)
	}

	// TTFT from the C=1 level
	ttft := levels[0].TTFTMs.P90

	derived := ComputeDerived(model, cfg.TargetConcurrency)

	profile := &Profile{
		Model:                cfg.Model,
		CreatedAt:            time.Now(),
		WorkloadISL:          cfg.WorkloadISL,
		WorkloadOSL:          cfg.WorkloadOSL,
		TargetConcurrency:    cfg.TargetConcurrency,
		KernelWarmupRequests: kernelCount,
		TTFTMs:               ttft,
		TPOTModel:            model,
		Derived:              derived,
		Levels:               levels,
	}

	return profile, nil
}

type probeResult struct {
	ttft float64 // ms
	tpot float64 // ms (mean ITL)
}

// detectKernelWarmup sends sequential requests at C=1 until TTFT stabilizes.
func detectKernelWarmup(ctx context.Context, cl *client.Client, cfg *CharacterizeConfig) (int, []probeResult, error) {
	var ttfts []float64
	var results []probeResult

	for i := 0; i < cfg.MaxKernelRequests; i++ {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}

		result := sendProbe(ctx, cl, cfg)
		if result == nil {
			continue
		}

		ttfts = append(ttfts, result.ttft)
		results = append(results, *result)

		slog.Info("Kernel probe",
			"request", i+1,
			"ttft_ms", fmt.Sprintf("%.1f", result.ttft),
			"tpot_ms", fmt.Sprintf("%.2f", result.tpot))

		if IsStable(ttfts, cfg.StabilityThreshold, cfg.StabilityWindow) {
			return len(ttfts), results, nil
		}
	}

	slog.Warn("Kernel warmup did not stabilize, using max probes",
		"max", cfg.MaxKernelRequests)
	return len(ttfts), results, nil
}

// sendProbe sends a single request and returns timing data.
func sendProbe(ctx context.Context, cl *client.Client, cfg *CharacterizeConfig) *probeResult {
	conv := cfg.Dataset.NextConversation()

	var result *client.Result
	if conv.Prompt != "" {
		req := &client.CompletionRequest{
			Model:       cfg.Model,
			Prompt:      conv.Prompt,
			Stream:      true,
			MaxTokens:   conv.MaxTokens,
			Stop:        conv.Stop,
			Temperature: conv.Temperature,
		}
		if cfg.CacheSalt != nil && cfg.CacheSalt.Mode == "fixed" {
			req.CacheSalt = cfg.CacheSalt.Value
		}
		result = cl.CompletionStream(ctx, req)
	} else {
		msgs := conv.Turns[0]
		req := &client.Request{
			Model:     cfg.Model,
			Messages:  msgs,
			Stream:    true,
			MaxTokens: conv.MaxTokens,
		}
		if cfg.CacheSalt != nil && cfg.CacheSalt.Mode == "fixed" {
			req.CacheSalt = cfg.CacheSalt.Value
		}
		result = cl.ChatStream(ctx, req)
	}

	if result.Err != nil {
		slog.Warn("Probe request failed", "error", result.Err)
		return nil
	}

	ttft := result.TTFT().Seconds() * 1000
	itls := result.ITLs()
	tpot := 0.0
	if len(itls) > 0 {
		sum := 0.0
		for _, d := range itls {
			sum += d.Seconds() * 1000
		}
		tpot = sum / float64(len(itls))
	}

	return &probeResult{ttft: ttft, tpot: tpot}
}

// measureLevels collects measurements at D-optimal points {1, target/2, target}.
func measureLevels(ctx context.Context, cfg *CharacterizeConfig, cl *client.Client, kernelResults []probeResult) []Level {
	// Determine concurrency levels to measure
	concurrencies := dOptimalLevels(cfg.TargetConcurrency)

	// C=1 data from kernel warmup probes (use post-warmup requests)
	c1Level := levelFromProbes(1, kernelResults, cfg.DiscardPerLevel)

	var levels []Level
	if c1Level != nil {
		levels = append(levels, *c1Level)
	}

	// Skip concurrent measurements if target is 1 (already have C=1)
	if cfg.TargetConcurrency <= 1 {
		return levels
	}

	// Estimate request time at C=1 for duration planning
	reqTimeC1 := 100.0 // fallback: 100ms
	if c1Level != nil {
		reqTimeC1 = c1Level.TTFTMs.Mean + c1Level.TPOTMs.Mean*float64(cfg.WorkloadOSL)
	}

	// Build stages for concurrent levels (skip C=1, already measured)
	var stages []loadgen.Stage
	var stageConcurrencies []int
	for _, c := range concurrencies {
		if c <= 1 {
			continue
		}
		// Duration: enough for requestsPerStream requests per stream
		// With C concurrent streams, each producing ~1 request per reqTime
		// Scale estimated request time linearly with concurrency (rough)
		estReqTime := reqTimeC1 * (1 + 0.5*float64(c)/float64(cfg.TargetConcurrency))
		durMs := float64(cfg.RequestsPerStream) * estReqTime * 2
		dur := time.Duration(durMs * float64(time.Millisecond))
		if dur < 3*time.Second {
			dur = 3 * time.Second
		}
		stages = append(stages, loadgen.Stage{
			Concurrency: c,
			Duration:    dur,
		})
		stageConcurrencies = append(stageConcurrencies, c)
	}

	if len(stages) == 0 {
		return levels
	}

	// Run all concurrent levels using a single Generator + RunStages
	recorders := make([]*recorder.Recorder, len(stages))
	gen := &loadgen.Generator{
		Target:    cfg.Target,
		Model:     cfg.Model,
		Dataset:   cfg.Dataset,
		Recorder:  recorder.NewMemory(), // initial, will be swapped
		CacheSalt: cfg.CacheSalt,
	}

	slog.Info("Phase 2: concurrent measurement",
		"levels", stageConcurrencies)

	gen.RunStages(ctx, stages, func(i, concurrency int) {
		rec := recorder.NewMemory()
		recorders[i] = rec
		gen.SetRecorder(rec)
		slog.Info("Measuring", "concurrency", concurrency)
	})

	// Compute stats for each level
	for i, rec := range recorders {
		if rec == nil {
			continue
		}
		rec.Close()
		records := rec.Records()
		level := levelFromRecords(stageConcurrencies[i], records, cfg.DiscardPerLevel)
		if level != nil {
			levels = append(levels, *level)
		}
	}

	return levels
}

// dOptimalLevels returns the D-optimal measurement points for a quadratic
// model on [1, target]: {1, target/2, target}.
func dOptimalLevels(target int) []int {
	if target <= 1 {
		return []int{1}
	}
	if target <= 3 {
		return []int{1, target}
	}
	mid := target / 2
	return []int{1, mid, target}
}

// levelFromProbes creates a Level from sequential probe results.
func levelFromProbes(concurrency int, results []probeResult, discard int) *Level {
	if discard >= len(results) {
		// Not enough results after discarding warmup
		if len(results) > 0 {
			discard = 0 // use all if we can't discard
		} else {
			return nil
		}
	}
	measured := results[discard:]
	if len(measured) == 0 {
		return nil
	}

	var ttfts, tpots []float64
	for _, r := range measured {
		ttfts = append(ttfts, r.ttft)
		tpots = append(tpots, r.tpot)
	}

	return &Level{
		Concurrency: concurrency,
		Requests:    len(measured),
		TTFTMs:      computeStats(ttfts),
		TPOTMs:      computeStats(tpots),
	}
}

// levelFromRecords creates a Level from recorder records.
func levelFromRecords(concurrency int, records []recorder.Record, discard int) *Level {
	// Filter to OK records only
	var ok []recorder.Record
	for _, r := range records {
		if r.Status == "ok" {
			ok = append(ok, r)
		}
	}

	if discard >= len(ok) {
		if len(ok) > 0 {
			discard = 0
		} else {
			return nil
		}
	}
	measured := ok[discard:]
	if len(measured) == 0 {
		return nil
	}

	var ttfts, tpots []float64
	for _, r := range measured {
		ttfts = append(ttfts, r.TTFT)
		if len(r.ITLs) > 0 {
			sum := 0.0
			for _, itl := range r.ITLs {
				sum += itl
			}
			tpots = append(tpots, sum/float64(len(r.ITLs)))
		}
	}

	return &Level{
		Concurrency: concurrency,
		Requests:    len(measured),
		TTFTMs:      computeStats(ttfts),
		TPOTMs:      computeStats(tpots),
	}
}

// computeStats computes mean, p50, p90 from a slice of values.
func computeStats(values []float64) LatencyStats {
	if len(values) == 0 {
		return LatencyStats{}
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}

	return LatencyStats{
		Mean: sum / float64(len(sorted)),
		P50:  percentile(sorted, 0.50),
		P90:  percentile(sorted, 0.90),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
