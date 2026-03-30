package warmup

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
)

// Stages converts warmup config stages into loadgen stages. Stages with
// concurrency=0 inherit defaultConcurrency (typically from the first main stage).
func Stages(cfgStages []config.WarmupStage, defaultConcurrency int) ([]loadgen.Stage, error) {
	if len(cfgStages) == 0 {
		return nil, fmt.Errorf("warmup: no stages configured")
	}

	stages := make([]loadgen.Stage, len(cfgStages))
	for i, ws := range cfgStages {
		dur := ws.Duration.Duration()
		if dur <= 0 {
			return nil, fmt.Errorf("warmup stage %d: duration must be > 0", i)
		}

		concurrency := ws.Concurrency
		if concurrency <= 0 {
			concurrency = defaultConcurrency
		}

		var rampup time.Duration
		if ws.Stagger {
			rampup = dur
		}

		slog.Info("Warmup stage",
			"stage", fmt.Sprintf("%d/%d", i+1, len(cfgStages)),
			"concurrency", concurrency,
			"duration", dur,
			"stagger", ws.Stagger)

		stages[i] = loadgen.Stage{
			Concurrency: concurrency,
			Duration:    dur,
			Rampup:      rampup,
		}
	}

	return stages, nil
}
