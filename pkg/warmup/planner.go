package warmup

import (
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
)

// PlanConfig controls warmup stage generation from a profile.
type PlanConfig struct {
	TargetConcurrency int
	WorkloadOSL       int
	SettleCycles      int           // full request cycles after ramp before recording (default: 2)
	MinSettle         time.Duration // minimum settle duration (default: 5s)
	MaxSettle         time.Duration // maximum settle duration (default: 120s)
}

func (c *PlanConfig) defaults() {
	if c.SettleCycles <= 0 {
		c.SettleCycles = 2
	}
	if c.MinSettle <= 0 {
		c.MinSettle = 5 * time.Second
	}
	if c.MaxSettle <= 0 {
		c.MaxSettle = 120 * time.Second
	}
}

// PlanWarmupStages computes warmup stages from a characterization profile.
// Returns 2 stages:
//  1. Kernel warmup at C=1 (no rampup)
//  2. Settle at target concurrency with staggered rampup
//
// The settle stage staggers stream starts across one request lifetime so that
// by the time recording begins, the concurrent streams are evenly distributed
// across their request lifecycle — producing true steady state (constant KV
// cache usage, constant TPOT) rather than synchronized waves.
func PlanWarmupStages(profile *Profile, cfg *PlanConfig) []loadgen.Stage {
	cfg.defaults()

	osl := float64(cfg.WorkloadOSL)
	if osl <= 0 {
		osl = float64(profile.WorkloadOSL)
	}

	// Estimate single-request duration at C=1 (ms)
	reqTimeC1 := profile.TTFTMs + profile.TPOTModel.Predict(1)*osl

	// Stage 1: Kernel warmup at C=1
	// Duration = enough time for kernel_warmup_requests to complete, with 50% headroom
	kernelMs := float64(profile.KernelWarmupRequests) * reqTimeC1 * 1.5
	kernelDur := time.Duration(kernelMs * float64(time.Millisecond))
	if kernelDur < 2*time.Second {
		kernelDur = 2 * time.Second
	}

	// Estimate request lifetime at target concurrency
	reqTimeTarget := profile.TTFTMs + profile.TPOTModel.Predict(float64(cfg.TargetConcurrency))*osl
	requestLifetime := time.Duration(reqTimeTarget * float64(time.Millisecond))

	// Stage 2: Settle at target concurrency
	// Rampup = one request lifetime (stagger streams across one full lifecycle)
	// Duration = rampup + settle_cycles × request_lifetime
	//   - During rampup: streams start in staggered fashion
	//   - After rampup: all streams running, evenly distributed across lifecycle
	//   - settle_cycles more lifetimes for things to stabilize
	rampup := requestLifetime
	settleDur := rampup + time.Duration(cfg.SettleCycles)*requestLifetime
	if settleDur < cfg.MinSettle {
		settleDur = cfg.MinSettle
	}
	if settleDur > cfg.MaxSettle {
		settleDur = cfg.MaxSettle
		// Don't let rampup exceed total duration
		if rampup > settleDur/2 {
			rampup = settleDur / 2
		}
	}

	return []loadgen.Stage{
		{Concurrency: 1, Duration: kernelDur},
		{Concurrency: cfg.TargetConcurrency, Duration: settleDur, Rampup: rampup},
	}
}
