package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config defines a complete benchmark run.
type Config struct {
	Load     Load     `json:"load"`
	Workload Workload `json:"workload"`
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
	Type       string `json:"type"`                  // synthetic, faker, corpus
	ISL        int    `json:"isl"`                   // input sequence length (tokens)
	OSL        int    `json:"osl"`                   // output sequence length (tokens)
	Turns      int    `json:"turns"`                 // turns per conversation
	CorpusPath string `json:"corpus_path,omitempty"` // path to corpus file/directory
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
