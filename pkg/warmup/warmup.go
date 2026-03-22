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
	Rampup            time.Duration // if set, skip probing and use this directly

	MaxKernelRequests  int     // cap on kernel warmup probes (default: 10)
	StabilityThreshold float64 // TTFT change threshold (default: 0.10)
	StabilityWindow    int     // consecutive stable readings (default: 2)
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
}

// ComputeStages returns warmup stages that stagger stream starts across one
// request lifetime for true steady state.
//
// If Rampup is set, skips probing and uses it directly.
// Otherwise, probes the engine to detect kernel compilation and measure
// request lifetime.
func ComputeStages(ctx context.Context, cfg *AutoConfig) ([]loadgen.Stage, error) {
	cfg.defaults()

	rampup := cfg.Rampup
	if rampup > 0 {
		slog.Info("Warmup using configured rampup, skipping probes", "rampup", rampup)
	} else {
		// Probe the engine to measure request lifetime
		measured, err := probeRequestLifetime(ctx, cfg)
		if err != nil {
			return nil, err
		}
		rampup = measured
	}

	settleDur := rampup
	if settleDur < 5*time.Second {
		settleDur = 5 * time.Second
	}
	if settleDur > 120*time.Second {
		settleDur = 120 * time.Second
		rampup = settleDur
	}

	stages := []loadgen.Stage{
		{Concurrency: cfg.TargetConcurrency, Duration: settleDur, Rampup: rampup},
	}

	slog.Info("Warmup settle stage",
		"concurrency", cfg.TargetConcurrency,
		"duration", settleDur,
		"rampup", rampup)

	return stages, nil
}

// probeRequestLifetime sends sequential requests to detect kernel warmup
// and measure request lifetime. Returns the measured lifetime as a duration.
func probeRequestLifetime(ctx context.Context, cfg *AutoConfig) (time.Duration, error) {
	cl := client.New(cfg.Target)

	var ttfts []float64
	var ttftStable, tpotSum float64
	var tpotCount int

	for i := 0; i < cfg.MaxKernelRequests; i++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
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
			if tpotCount == 0 {
				ttftStable = ttft
			}
			tpotSum += tpot
			tpotCount++
			if tpotCount >= 2 {
				break
			}
		} else if len(ttfts) > cfg.StabilityWindow {
			tpotSum += tpot
			tpotCount++
		}
	}

	if tpotCount == 0 || ttftStable == 0 {
		if len(ttfts) > 0 {
			ttftStable = ttfts[len(ttfts)-1]
		}
		tpotSum = 1
		tpotCount = 1
	}

	meanTPOT := tpotSum / float64(tpotCount)
	requestLifetimeMs := ttftStable + meanTPOT*float64(cfg.WorkloadOSL)
	requestLifetime := time.Duration(requestLifetimeMs * float64(time.Millisecond))

	rounded := requestLifetime.Round(time.Second)
	if rounded == 0 {
		rounded = requestLifetime.Round(time.Millisecond)
	}
	slog.Info("Warmup probing complete",
		"ttft_ms", fmt.Sprintf("%.1f", ttftStable),
		"tpot_ms", fmt.Sprintf("%.2f", meanTPOT),
		"request_lifetime", requestLifetime)
	slog.Info("To skip probing next time, use:",
		"warmup", fmt.Sprintf(`{"auto":true,"rampup":"%s"}`, rounded))

	return requestLifetime, nil
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
