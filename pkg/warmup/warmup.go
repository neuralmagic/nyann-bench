package warmup

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
)

// Config controls the warmup behavior.
type Config struct {
	Duration    time.Duration
	Concurrency int
	Stagger     bool
}

// Stage returns a single warmup stage that runs traffic at the target
// concurrency for the configured duration, allowing the engine to JIT-compile
// kernels before measurement begins. If Stagger is true, stream starts are
// spread evenly across the warmup duration.
func Stage(cfg *Config) (loadgen.Stage, error) {
	if cfg.Duration <= 0 {
		return loadgen.Stage{}, fmt.Errorf("warmup duration must be > 0")
	}
	if cfg.Concurrency <= 0 {
		return loadgen.Stage{}, fmt.Errorf("warmup concurrency must be > 0")
	}

	var rampup time.Duration
	if cfg.Stagger {
		rampup = cfg.Duration
	}

	slog.Info("Warmup stage",
		"concurrency", cfg.Concurrency,
		"duration", cfg.Duration,
		"stagger", cfg.Stagger)

	return loadgen.Stage{
		Concurrency: cfg.Concurrency,
		Duration:    cfg.Duration,
		Rampup:      rampup,
	}, nil
}
