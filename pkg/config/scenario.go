package config

import (
	"time"
)

// ScenarioConfig is the universal intermediate representation for benchmark
// configurations. Both JSON configs and Starlark scripts produce this type.
type ScenarioConfig struct {
	Target   string          // default target URL (empty = use CLI flag)
	Model    string          // default model (empty = use CLI flag)
	Workload Workload        // default workload for stages that don't override
	Stages   []ScenarioStage // ordered stages to execute
	Sync     *SyncConfig     // barrier sync config (nil = no sync)
	Workers  int             // total workers for load division (from --workers flag)
	WorkerID int             // this worker's index (from --worker-id or JOB_COMPLETION_INDEX)
}

// SyncConfig configures distributed barrier synchronization across pods.
type SyncConfig struct {
	Workers int      `json:"workers"`           // expected number of pods
	Timeout Duration `json:"timeout,omitempty"` // max wait per barrier (default 10m)
	Port    int      `json:"port,omitempty"`    // barrier server port (default 8080)
	Addr    string   `json:"addr,omitempty"`    // barrier server address (auto-detected from BARRIER_ADDR)
}

// ScenarioStage is a single phase of a benchmark with optional per-stage overrides.
type ScenarioStage struct {
	Name         string        // human-readable label (for logging/analysis)
	Duration     time.Duration // how long this stage runs
	Mode         string        // "concurrent", "constant", "poisson" (empty = inherit)
	Concurrency  int           // concurrent streams (0 = inherit)
	Rate         float64       // req/s for constant/poisson (0 = inherit)
	MaxInFlight  int           // cap for rate-based modes (0 = unlimited)
	Rampup       time.Duration // stagger stream starts / ramp rate
	Workload     *Workload     // nil = inherit from scenario
	Target       string        // empty = inherit from scenario
	Model        string        // empty = inherit from scenario
	MaxRequests  int           // stop after this many requests (0 = unlimited)
	Warmup       bool          // true = don't record results
	Barrier      bool          // true = sync point (other fields ignored)
	BarrierDrain bool          // true = stop pool before sync, fresh pool after
}

// ToScenarioConfig converts a JSON Config into the universal ScenarioConfig IR.
func (c *Config) ToScenarioConfig() *ScenarioConfig {
	sc := &ScenarioConfig{
		Workload: c.Workload,
	}

	// Convert warmup to a warmup stage if present
	effectiveStages := c.EffectiveStages()
	if c.Warmup != nil && c.Warmup.Duration.Duration() > 0 {
		var rampup time.Duration
		if c.Warmup.Stagger {
			rampup = c.Warmup.Duration.Duration()
		}
		warmupConcurrency := 0
		if len(effectiveStages) > 0 {
			warmupConcurrency = effectiveStages[0].Concurrency
		}
		sc.Stages = append(sc.Stages, ScenarioStage{
			Name:        "warmup",
			Duration:    c.Warmup.Duration.Duration(),
			Mode:        c.Load.Mode,
			Concurrency: warmupConcurrency,
			Rampup:      rampup,
			Warmup:      true,
		})
	}

	for _, s := range effectiveStages {
		sc.Stages = append(sc.Stages, ScenarioStage{
			Duration:    s.Duration.Duration(),
			Mode:        c.Load.Mode,
			Concurrency: s.Concurrency,
			Rate:        c.Load.Rate,
			MaxInFlight: c.Load.MaxInFlight,
			MaxRequests: s.MaxRequests,
			Rampup:      c.Load.Rampup.Duration(),
		})
	}

	return sc
}

// DivideConcurrency returns the concurrency share for workerID out of nWorkers.
// Remainder is distributed to lower-indexed workers.
func DivideConcurrency(total, nWorkers, workerID int) int {
	if nWorkers <= 1 {
		return total
	}
	base := total / nWorkers
	if workerID < total%nWorkers {
		return base + 1
	}
	return base
}

// DivideRate returns the rate share for one worker.
func DivideRate(total float64, nWorkers int) float64 {
	if nWorkers <= 1 {
		return total
	}
	return total / float64(nWorkers)
}

// InsertImplicitBarrier adds a barrier before the first non-warmup stage
// if one doesn't already exist at that position. This is called when --workers > 1
// to ensure a sync point even without explicit barrier() calls.
func (sc *ScenarioConfig) InsertImplicitBarrier() {
	// Find the index of the first non-warmup stage
	firstMeasured := -1
	for i, s := range sc.Stages {
		if !s.Warmup && !s.Barrier {
			firstMeasured = i
			break
		}
	}
	if firstMeasured < 0 {
		return // no measured stages
	}

	// Check if there's already a barrier right before it
	if firstMeasured > 0 && sc.Stages[firstMeasured-1].Barrier {
		return // explicit barrier already present
	}

	// Insert implicit barrier with drain=true (clean break after warmup)
	barrier := ScenarioStage{Barrier: true, BarrierDrain: true}
	sc.Stages = append(sc.Stages[:firstMeasured], append([]ScenarioStage{barrier}, sc.Stages[firstMeasured:]...)...)
}
