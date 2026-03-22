package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config defines a complete benchmark run.
// Use "load" for a single stage, "stages" for explicit steps, or "sweep" for a smooth ramp.
type Config struct {
	Load     Load     `json:"load"`
	Stages   []Stage  `json:"stages,omitempty"`
	Sweep    *Sweep   `json:"sweep,omitempty"`
	Warmup   *Warmup  `json:"warmup,omitempty"`
	Workload Workload `json:"workload"`
}

// Warmup probes the engine and staggers stream starts across one request
// lifetime so the batch reaches true steady state before recording.
// If Rampup is set, probing is skipped and the value is used directly.
type Warmup struct {
	Rampup Duration `json:"rampup,omitempty"` // skip probing, use this as stagger duration
}

// Stage defines one step in a multi-stage sweep.
type Stage struct {
	Concurrency int      `json:"concurrency"`
	Duration    Duration `json:"duration"`
}

// Sweep defines a smooth concurrency ramp from Min to Max over Steps stages.
type Sweep struct {
	Min          int      `json:"min"`
	Max          int      `json:"max"`
	Steps        int      `json:"steps"`
	StepDuration Duration `json:"step_duration"`
}

// Load defines how requests are scheduled.
type Load struct {
	Mode        string   `json:"mode"`         // concurrent, constant, poisson
	Concurrency int      `json:"concurrency"`  // concurrent mode: number of streams
	Rate        float64  `json:"rate"`          // constant/poisson mode: requests per second
	MaxInFlight int      `json:"max_inflight"`  // constant/poisson mode: cap on concurrent requests (0=unlimited)
	Rampup      Duration `json:"rampup"`        // stagger streams or ramp rate
	Duration    Duration `json:"duration"`      // total benchmark duration
}

// Workload defines the dataset and request parameters.
type Workload struct {
	Type          string  `json:"type"`                    // synthetic, faker, corpus, gsm8k
	Name          string  `json:"name,omitempty"`          // human-readable name for this workload (shown in Prometheus/Grafana)
	ISL           int     `json:"isl"`                     // input sequence length (tokens)
	OSL           int     `json:"osl"`                     // output sequence length (tokens)
	Turns         int     `json:"turns"`                   // turns per conversation
	CorpusPath    string  `json:"corpus_path,omitempty"`   // path to corpus file/directory
	GSM8KPath      string `json:"gsm8k_path,omitempty"`       // path to GSM8K test JSONL file
	GSM8KTrainPath string `json:"gsm8k_train_path,omitempty"` // path to GSM8K training JSONL (for few-shot examples)
	NumFewShot     *int   `json:"num_fewshot,omitempty"`       // number of few-shot examples (default: 5, requires gsm8k_train_path)
	CharsPerToken float64 `json:"chars_per_token"`         // override auto-calibrated ratio (0 = auto)
	CacheSalt *CacheSalt `json:"cache_salt,omitempty"` // prefix cache isolation config
}

// CacheSalt configures vLLM prefix cache isolation.
//   - {"mode": "random"}                    → unique 256-bit salt per request
//   - {"mode": "fixed", "value": "abc123"}  → same salt on every request
type CacheSalt struct {
	Mode  string `json:"mode"`            // "random" or "fixed"
	Value string `json:"value,omitempty"` // salt value (required when mode is "fixed")
}

// Parse reads a config from a JSON string or file path.
func Parse(input string) (*Config, error) {
	var data []byte

	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "{") {
		data = []byte(input)
	} else {
		var err error
		data, err = os.ReadFile(input)
		if err != nil {
			return nil, fmt.Errorf("reading config file %s: %w", input, err)
		}
	}

	cfg := &Config{
		Load: Load{
			Mode:        "concurrent",
			Concurrency: 10,
			Rate:        10.0,
			Duration:    Duration(60 * time.Second),
		},
		Workload: Workload{
			Type:  "faker",
			ISL:   128,
			OSL:   256,
			Turns: 1,
		},
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}

// Duration is a time.Duration that marshals/unmarshals as a JSON string ("60s", "10m").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Try as number (seconds)
		var secs float64
		if err2 := json.Unmarshal(b, &secs); err2 != nil {
			return fmt.Errorf("duration must be a string (\"60s\") or number (seconds): %w", err)
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// EffectiveStages returns the stages to run.
// Priority: sweep > stages > single load config.
func (c *Config) EffectiveStages() []Stage {
	if c.Sweep != nil {
		return SweepStages(c.Sweep.Min, c.Sweep.Max, c.Sweep.Steps, c.Sweep.StepDuration)
	}
	if len(c.Stages) > 0 {
		return c.Stages
	}
	return []Stage{{
		Concurrency: c.Load.Concurrency,
		Duration:    c.Load.Duration,
	}}
}

// SweepStages generates N evenly-spaced concurrency stages from min to max.
func SweepStages(minC, maxC, steps int, stageDuration Duration) []Stage {
	if steps < 2 {
		return []Stage{{Concurrency: maxC, Duration: stageDuration}}
	}
	stages := make([]Stage, steps)
	for i := 0; i < steps; i++ {
		c := minC + (maxC-minC)*i/(steps-1)
		if c < 1 {
			c = 1
		}
		stages[i] = Stage{Concurrency: c, Duration: stageDuration}
	}
	return stages
}
