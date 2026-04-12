package analysis

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// Summary holds aggregate statistics from one or more JSONL result files.
type Summary struct {
	TotalRequests   int     `json:"total_requests"`
	SuccessRequests int     `json:"successful_requests"`
	ErrorRequests   int     `json:"error_requests"`
	TotalDurationS  float64 `json:"total_duration_seconds"`
	RequestsPerSec  float64 `json:"requests_per_second"`

	TotalOutputTokens int     `json:"total_output_tokens"`
	TotalPromptTokens int     `json:"total_prompt_tokens"`
	OutputTokensPerS  float64 `json:"output_tokens_per_second"`

	TTFTMs  LatencyStats `json:"ttft_ms"`
	ITLMs   LatencyStats `json:"itl_ms"`
	E2EMs   LatencyStats `json:"e2e_latency_ms"`

	Conversations int          `json:"conversations"`
	TurnsPerConv  LatencyStats `json:"turns_per_conversation"`

	// Eval stats (populated when dataset provides expected answers)
	EvalTotal     int     `json:"eval_total,omitempty"`
	EvalCorrect   int     `json:"eval_correct,omitempty"`
	EvalIncorrect int     `json:"eval_incorrect,omitempty"`
	EvalAccuracy  float64 `json:"eval_accuracy,omitempty"`

	Timestamps *recorder.Timestamps `json:"timestamps,omitempty"`
}

// LatencyStats holds percentile statistics for a latency metric.
type LatencyStats struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// LoadRecords reads all JSONL files matching the pattern in a directory.
func LoadRecords(dir string) ([]recorder.Record, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "requests_*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no requests_*.jsonl files found in %s", dir)
	}

	var all []recorder.Record
	for _, path := range matches {
		records, err := loadJSONL(path)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
		all = append(all, records...)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].StartTime < all[j].StartTime
	})
	return all, nil
}

func loadJSONL(path string) ([]recorder.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []recorder.Record
	dec := json.NewDecoder(f)
	for {
		var r recorder.Record
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

// LoadTimestamps reads all timestamp files and returns the merged measurement window.
func LoadTimestamps(dir string) (startTime, endTime float64, err error) {
	matches, err := filepath.Glob(filepath.Join(dir, "timestamps_*.json"))
	if err != nil {
		return 0, 0, err
	}
	if len(matches) == 0 {
		return 0, 0, fmt.Errorf("no timestamps_*.json files found in %s", dir)
	}

	first := true
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, 0, err
		}
		var ts recorder.Timestamps
		if err := json.Unmarshal(data, &ts); err != nil {
			return 0, 0, err
		}
		if first {
			startTime = ts.RampupEndTime
			endTime = ts.EndTime
			first = false
		} else {
			if ts.RampupEndTime > startTime {
				startTime = ts.RampupEndTime
			}
			if ts.EndTime < endTime {
				endTime = ts.EndTime
			}
		}
	}
	return startTime, endTime, nil
}

// Compute generates summary statistics from records.
// If startTime/endTime are non-zero, only records within that window are included.
func Compute(records []recorder.Record, startTime, endTime float64) *Summary {
	// Filter to measurement window if specified
	if startTime > 0 && endTime > 0 {
		var filtered []recorder.Record
		for _, r := range records {
			if r.StartTime >= startTime && r.EndTime <= endTime {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}

	s := &Summary{}
	if len(records) == 0 {
		return s
	}

	var ttfts, e2es, allITLs []float64
	convs := map[string]int{}

	minT, maxT := records[0].StartTime, records[0].EndTime
	for _, r := range records {
		s.TotalRequests++
		if r.Status == "ok" {
			s.SuccessRequests++
			ttfts = append(ttfts, r.TTFT)
			e2es = append(e2es, r.TotalLatencyMs)
			allITLs = append(allITLs, r.ITLs...)
			s.TotalOutputTokens += r.OutputTokens
			s.TotalPromptTokens += r.PromptTokens
		} else {
			s.ErrorRequests++
		}

		if r.StartTime < minT {
			minT = r.StartTime
		}
		if r.EndTime > maxT {
			maxT = r.EndTime
		}

		convs[r.ConversationID]++
	}

	s.TotalDurationS = maxT - minT
	if s.TotalDurationS > 0 {
		s.RequestsPerSec = float64(s.SuccessRequests) / s.TotalDurationS
		s.OutputTokensPerS = float64(s.TotalOutputTokens) / s.TotalDurationS
	}

	s.TTFTMs = computeLatencyStats(ttfts)
	s.ITLMs = computeLatencyStats(allITLs)
	s.E2EMs = computeLatencyStats(e2es)

	s.Conversations = len(convs)
	var turnsPerConv []float64
	for _, count := range convs {
		turnsPerConv = append(turnsPerConv, float64(count))
	}
	s.TurnsPerConv = computeLatencyStats(turnsPerConv)

	// Eval stats
	for _, r := range records {
		if r.EvalCorrect != nil {
			s.EvalTotal++
			if *r.EvalCorrect {
				s.EvalCorrect++
			} else {
				s.EvalIncorrect++
			}
		}
	}
	if s.EvalTotal > 0 {
		s.EvalAccuracy = float64(s.EvalCorrect) / float64(s.EvalTotal)
	}

	return s
}

func computeLatencyStats(values []float64) LatencyStats {
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
		P99:  percentile(sorted, 0.99),
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// FormatSummary returns a human-readable summary string.
func FormatSummary(s *Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Benchmark Results ===\n")
	fmt.Fprintf(&b, "Requests:       %d total, %d ok, %d errors\n", s.TotalRequests, s.SuccessRequests, s.ErrorRequests)
	fmt.Fprintf(&b, "Conversations:  %d (avg %.1f turns)\n", s.Conversations, s.TurnsPerConv.Mean)
	fmt.Fprintf(&b, "Duration:       %.1fs\n", s.TotalDurationS)
	fmt.Fprintf(&b, "Throughput:     %.1f req/s, %.0f output tok/s\n", s.RequestsPerSec, s.OutputTokensPerS)
	fmt.Fprintf(&b, "Tokens:         %d prompt, %d output\n", s.TotalPromptTokens, s.TotalOutputTokens)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "TTFT (ms):      mean=%.1f  p50=%.1f  p90=%.1f  p99=%.1f  min=%.1f  max=%.1f\n",
		s.TTFTMs.Mean, s.TTFTMs.P50, s.TTFTMs.P90, s.TTFTMs.P99, s.TTFTMs.Min, s.TTFTMs.Max)
	fmt.Fprintf(&b, "ITL  (ms):      mean=%.2f  p50=%.2f  p90=%.2f  p99=%.2f  min=%.2f  max=%.2f\n",
		s.ITLMs.Mean, s.ITLMs.P50, s.ITLMs.P90, s.ITLMs.P99, s.ITLMs.Min, s.ITLMs.Max)
	fmt.Fprintf(&b, "E2E  (ms):      mean=%.1f  p50=%.1f  p90=%.1f  p99=%.1f  min=%.1f  max=%.1f\n",
		s.E2EMs.Mean, s.E2EMs.P50, s.E2EMs.P90, s.E2EMs.P99, s.E2EMs.Min, s.E2EMs.Max)
	if s.EvalTotal > 0 {
		fmt.Fprintf(&b, "\n")
		fmt.Fprintf(&b, "Eval:           %d total, %d correct, %d incorrect\n",
			s.EvalTotal, s.EvalCorrect, s.EvalIncorrect)
		fmt.Fprintf(&b, "Accuracy:       %.1f%%\n", s.EvalAccuracy*100)
	}
	return b.String()
}
