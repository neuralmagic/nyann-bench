package warmup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
)

// AutoConfig controls the auto warmup behavior.
type AutoConfig struct {
	Target            string
	Model             string
	Dataset           dataset.Dataset
	TargetConcurrency int
	WorkloadOSL       int
	CacheSalt         *config.CacheSalt

	MaxKernelRequests  int     // cap on kernel warmup probes (default: 10)
	StabilityThreshold float64 // TTFT change threshold (default: 0.10)
	StabilityWindow    int     // consecutive stable readings (default: 2)
	SettleCycles       int     // request cycles after ramp before recording (default: 2)
}

func (c *AutoConfig) defaults() {
	if c.MaxKernelRequests <= 0 {
		c.MaxKernelRequests = 10
	}
	if c.StabilityThreshold <= 0 {
		c.StabilityThreshold = 0.10
	}
	if c.StabilityWindow <= 0 {
		c.StabilityWindow = 2
	}
	if c.SettleCycles <= 0 {
		c.SettleCycles = 2
	}
}

// ComputeStages probes the engine with sequential requests to detect kernel
// warmup and measure request lifetime, then returns warmup stages that
// stagger stream starts across one request lifetime for true steady state.
func ComputeStages(ctx context.Context, cfg *AutoConfig) ([]loadgen.Stage, error) {
	cfg.defaults()

	cl := client.New(cfg.Target)

	// Phase 1: Send sequential probes to compile kernels and measure timing
	var ttfts []float64
	var ttftStable, tpotSum float64
	var tpotCount int
	kernelCount := 0

	for i := 0; i < cfg.MaxKernelRequests; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		ttft, tpot, err := sendProbe(ctx, cl, cfg)
		if err != nil {
			slog.Warn("Warmup probe failed", "error", err)
			continue
		}

		ttfts = append(ttfts, ttft)
		slog.Info("Warmup probe",
			"request", i+1,
			"ttft_ms", fmt.Sprintf("%.1f", ttft),
			"tpot_ms", fmt.Sprintf("%.2f", tpot))

		if IsStable(ttfts, cfg.StabilityThreshold, cfg.StabilityWindow) {
			kernelCount = len(ttfts)
			// Use post-warmup measurements for timing
			ttftStable = ttft
			tpotSum += tpot
			tpotCount++
			// Collect one more measurement for confidence
			if tpotCount >= 2 {
				break
			}
		} else if len(ttfts) > cfg.StabilityWindow {
			// Post-convergence measurement
			tpotSum += tpot
			tpotCount++
		}
	}

	if kernelCount == 0 {
		kernelCount = len(ttfts)
		slog.Warn("Kernel warmup did not converge, using all probes", "count", kernelCount)
	}
	if tpotCount == 0 || ttftStable == 0 {
		// Use last measurement as fallback
		if len(ttfts) > 0 {
			ttftStable = ttfts[len(ttfts)-1]
		}
		tpotSum = 1 // avoid division by zero
		tpotCount = 1
	}

	meanTPOT := tpotSum / float64(tpotCount)

	// Request lifetime at target concurrency (ms)
	// Approximate: TPOT scales with concurrency but we only measured at C=1.
	// Use the C=1 measurement as a lower bound — the actual lifetime will be
	// longer, which means our rampup stagger is conservative (slightly too short
	// rather than too long). This is fine: natural jitter decorrelates streams
	// over the settle cycles.
	requestLifetimeMs := ttftStable + meanTPOT*float64(cfg.WorkloadOSL)
	requestLifetime := time.Duration(requestLifetimeMs * float64(time.Millisecond))

	slog.Info("Warmup computed",
		"kernel_requests", kernelCount,
		"ttft_ms", fmt.Sprintf("%.1f", ttftStable),
		"tpot_ms", fmt.Sprintf("%.2f", meanTPOT),
		"request_lifetime", requestLifetime)

	// Stage 1: Kernel warmup at C=1
	kernelDur := time.Duration(float64(kernelCount) * requestLifetimeMs * 1.5 * float64(time.Millisecond))
	if kernelDur < 2*time.Second {
		kernelDur = 2 * time.Second
	}

	// Stage 2: Settle at target concurrency
	// Rampup = one request lifetime (stagger streams across lifecycle)
	// Duration = rampup + settle_cycles × request_lifetime
	rampup := requestLifetime
	settleDur := rampup + time.Duration(cfg.SettleCycles)*requestLifetime
	if settleDur < 5*time.Second {
		settleDur = 5 * time.Second
	}
	if settleDur > 120*time.Second {
		settleDur = 120 * time.Second
		if rampup > settleDur/2 {
			rampup = settleDur / 2
		}
	}

	stages := []loadgen.Stage{
		{Concurrency: 1, Duration: kernelDur},
		{Concurrency: cfg.TargetConcurrency, Duration: settleDur, Rampup: rampup},
	}

	slog.Info("Warmup stages",
		"kernel_stage", fmt.Sprintf("C=1 for %v", kernelDur),
		"settle_stage", fmt.Sprintf("C=%d for %v (rampup %v)", cfg.TargetConcurrency, settleDur, rampup))

	return stages, nil
}

// sendProbe sends a single request and returns TTFT (ms) and mean TPOT (ms).
func sendProbe(ctx context.Context, cl *client.Client, cfg *AutoConfig) (ttft, tpot float64, err error) {
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
		return 0, 0, result.Err
	}

	ttft = result.TTFT().Seconds() * 1000
	itls := result.ITLs()
	if len(itls) > 0 {
		sum := 0.0
		for _, d := range itls {
			sum += d.Seconds() * 1000
		}
		tpot = sum / float64(len(itls))
	}

	return ttft, tpot, nil
}
