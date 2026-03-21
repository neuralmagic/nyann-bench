package warmup

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// QuadModel represents a fitted polynomial: f(x) = A + B*x + C*x².
type QuadModel struct {
	A float64 `json:"a"` // intercept (weight read floor)
	B float64 `json:"b"` // linear coefficient (KV cache scaling)
	C float64 `json:"c"` // quadratic coefficient (compute saturation)
}

// Predict evaluates the polynomial at x.
func (m *QuadModel) Predict(x float64) float64 {
	return m.A + m.B*x + m.C*x*x
}

// LatencyStats holds summary statistics for a latency metric (ms).
type LatencyStats struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
}

// Level holds raw measurements at a single concurrency level.
type Level struct {
	Concurrency int          `json:"concurrency"`
	Requests    int          `json:"requests"`
	TTFTMs      LatencyStats `json:"ttft_ms"`
	TPOTMs      LatencyStats `json:"tpot_ms"`
}

// DerivedMetrics are computed analytically from the fitted TPOT model.
type DerivedMetrics struct {
	OptimalConcurrency int     `json:"optimal_concurrency"`   // C where throughput peaks: sqrt(a/c)
	MaxThroughputTokS  float64 `json:"max_throughput_tok_s"`  // C_opt / TPOT(C_opt)
	TwoxDegradationC   int     `json:"2x_degradation_c"`      // C where TPOT = 2 × baseline
	Regime             string  `json:"regime"`                 // "bandwidth-bound" or "compute-bound"
}

// Profile is the output of the characterize command. It captures
// the engine's performance characteristics for a specific workload.
type Profile struct {
	Model                string         `json:"model"`
	CreatedAt            time.Time      `json:"created_at"`
	WorkloadISL          int            `json:"workload_isl"`
	WorkloadOSL          int            `json:"workload_osl"`
	TargetConcurrency    int            `json:"target_concurrency"`
	KernelWarmupRequests int            `json:"kernel_warmup_requests"`
	TTFTMs               float64        `json:"ttft_ms"`
	TPOTModel            QuadModel      `json:"tpot_model"`
	Derived              DerivedMetrics `json:"derived"`
	Levels               []Level        `json:"levels"`
}

// SaveProfile writes a profile to a JSON file.
func SaveProfile(path string, p *Profile) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling profile: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadProfile reads a profile from a JSON file.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile %s: %w", path, err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing profile %s: %w", path, err)
	}
	return &p, nil
}
