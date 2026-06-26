package analysis

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/prometheus"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// StageSummary holds per-stage metrics computed from client-side records
// and optionally enriched with server-side Prometheus metrics.
type StageSummary struct {
	Concurrency       int          `json:"concurrency"`
	TotalRequests     int          `json:"total_requests"`
	SuccessRequests   int          `json:"successful_requests"`
	ErrorRequests     int          `json:"error_requests"`
	DurationS         float64      `json:"duration_seconds"`
	TotalOutputTokens int          `json:"total_output_tokens"`
	OutputTokensPerS  float64      `json:"output_tokens_per_second"`
	TTFTMs            LatencyStats `json:"ttft_ms"`
	ITLMs             LatencyStats `json:"itl_ms"`
	E2EMs             LatencyStats `json:"e2e_latency_ms"`

	// Server-side metrics from Prometheus (populated by QueryServerMetrics).
	Server *ServerMetrics `json:"server,omitempty"`
}

// ServerMetrics holds server-side metrics queried from Prometheus.
type ServerMetrics struct {
	TTFT          prometheus.LatencyStats `json:"ttft_seconds"`
	ITL           prometheus.LatencyStats `json:"itl_seconds"`
	OutputTokensP50 float64              `json:"output_tokens_per_second_p50"`
	OutputTokensMax float64              `json:"output_tokens_per_second_max"`
	DecodeKVP50   float64                `json:"decode_kv_pct_p50,omitempty"`
	DecodeKVMax   float64                `json:"decode_kv_pct_max,omitempty"`
}

// ComputePerStage computes client-side statistics for each stage from records.
func ComputePerStage(records []recorder.Record, stages []recorder.StageTimestamp) []StageSummary {
	var summaries []StageSummary
	for _, stage := range stages {
		var ttfts, e2es, allITLs []float64
		totalOK, totalErr, totalOutTok := 0, 0, 0
		minT, maxT := stage.EndTime, stage.StartTime

		for _, r := range records {
			if r.StartTime < stage.StartTime || r.EndTime > stage.EndTime {
				continue
			}
			if r.Status == "ok" {
				totalOK++
				ttfts = append(ttfts, r.TTFT)
				e2es = append(e2es, r.TotalLatencyMs)
				allITLs = append(allITLs, r.ITLs...)
				totalOutTok += r.OutputTokens
			} else {
				totalErr++
			}
			if r.StartTime < minT {
				minT = r.StartTime
			}
			if r.EndTime > maxT {
				maxT = r.EndTime
			}
		}

		dur := maxT - minT
		tokPerSec := 0.0
		if dur > 0 {
			tokPerSec = float64(totalOutTok) / dur
		}

		summaries = append(summaries, StageSummary{
			Concurrency:       stage.Concurrency,
			TotalRequests:     totalOK + totalErr,
			SuccessRequests:   totalOK,
			ErrorRequests:     totalErr,
			DurationS:         dur,
			TotalOutputTokens: totalOutTok,
			OutputTokensPerS:  tokPerSec,
			TTFTMs:            computeLatencyStats(ttfts),
			ITLMs:             computeLatencyStats(allITLs),
			E2EMs:             computeLatencyStats(e2es),
		})
	}
	return summaries
}

// trimStage returns the measurement window for a stage, skipping the first 20%
// (rampup noise) and last 5% (transition artifacts).
func trimStage(start, end time.Time) (time.Time, time.Time) {
	dur := end.Sub(start)
	return start.Add(time.Duration(float64(dur) * 0.20)),
		start.Add(time.Duration(float64(dur) * 0.95))
}

// QueryServerMetrics queries Prometheus for server-side metrics for each stage
// and populates the Server field on each StageSummary.
func QueryServerMetrics(client *prometheus.Client, stages []StageSummary, timestamps []recorder.StageTimestamp, deployName string) {
	if len(stages) != len(timestamps) {
		return
	}

	podFilter := deployName + ".*"

	var wg sync.WaitGroup
	for i := range stages {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ts := timestamps[idx]
			start := time.Unix(int64(ts.StartTime), int64((ts.StartTime-float64(int64(ts.StartTime)))*1e9))
			end := time.Unix(int64(ts.EndTime), int64((ts.EndTime-float64(int64(ts.EndTime)))*1e9))

			trimStart, trimEnd := trimStage(start, end)
			if trimEnd.Sub(trimStart) < 5*time.Second {
				trimStart, trimEnd = start, end
			}

			sm := &ServerMetrics{}

			var metricWg sync.WaitGroup
			metricWg.Add(3)

			go func() {
				defer metricWg.Done()
				stats, err := client.HistogramQuantile(
					"vllm:time_to_first_token_seconds_bucket", podFilter, trimStart, trimEnd)
				if err != nil {
					slog.Debug("Failed to query TTFT", "stage", idx, "error", err)
					return
				}
				sm.TTFT = stats
			}()

			go func() {
				defer metricWg.Done()
				stats, err := client.HistogramQuantile(
					"vllm:inter_token_latency_seconds_bucket", podFilter, trimStart, trimEnd)
				if err != nil {
					slog.Debug("Failed to query ITL", "stage", idx, "error", err)
					return
				}
				sm.ITL = stats
			}()

			go func() {
				defer metricWg.Done()
				q := fmt.Sprintf(`sum(rate(vllm:generation_tokens_total{pod=~"%s"}[10s]))`, podFilter)
				stats, err := client.QueryGaugeStats(q, trimStart, trimEnd)
				if err != nil {
					slog.Debug("Failed to query TOK/s", "stage", idx, "error", err)
					return
				}
				sm.OutputTokensP50 = stats.P50
				sm.OutputTokensMax = stats.Max
			}()

			metricWg.Wait()
			stages[idx].Server = sm
		}(i)
	}
	wg.Wait()
}

// FormatStageTable formats per-stage summaries as a table.
// When server metrics are available, it shows server-side TTFT, ITL, and TOK/s.
// Otherwise it shows client-side metrics.
func FormatStageTable(stages []StageSummary) string {
	if len(stages) == 0 {
		return ""
	}

	hasServer := stages[0].Server != nil

	var b strings.Builder
	if hasServer {
		fmt.Fprintf(&b, "%6s  %6s  %5s  %9s  %10s  %10s  %10s  %10s\n",
			"CONC", "OK", "ERR", "TOK/S", "TTFT_P50", "TTFT_P99", "ITL_P50", "ITL_P99")
		b.WriteString(strings.Repeat("-", 82))
		b.WriteByte('\n')
		for _, s := range stages {
			sm := s.Server
			fmt.Fprintf(&b, "%6d  %6d  %5d  %9.1f  %10s  %10s  %10s  %10s\n",
				s.Concurrency, s.SuccessRequests, s.ErrorRequests,
				sm.OutputTokensP50,
				fmtDuration(sm.TTFT.P50), fmtDuration(sm.TTFT.P99),
				fmtDuration(sm.ITL.P50), fmtDuration(sm.ITL.P99))
		}
	} else {
		fmt.Fprintf(&b, "%6s  %6s  %5s  %9s  %9s  %10s  %10s  %10s\n",
			"CONC", "OK", "ERR", "TOT_TOK", "TOK/S", "TTFT_P50", "ITL_P50", "E2E_P50")
		b.WriteString(strings.Repeat("-", 82))
		b.WriteByte('\n')
		for _, s := range stages {
			fmt.Fprintf(&b, "%6d  %6d  %5d  %9d  %9.1f  %10s  %10s  %10s\n",
				s.Concurrency, s.SuccessRequests, s.ErrorRequests,
				s.TotalOutputTokens, s.OutputTokensPerS,
				fmtMs(s.TTFTMs.P50), fmtMs(s.ITLMs.P50), fmtMs(s.E2EMs.P50))
		}
	}
	return b.String()
}

func fmtDuration(seconds float64) string {
	if seconds == 0 {
		return "-"
	}
	if seconds < 0.001 {
		return fmt.Sprintf("%.0fus", seconds*1e6)
	}
	if seconds < 1 {
		return fmt.Sprintf("%.1fms", seconds*1000)
	}
	return fmt.Sprintf("%.2fs", seconds)
}

func fmtMs(ms float64) string {
	if ms == 0 {
		return "-"
	}
	if ms < 1 {
		return fmt.Sprintf("%.0fus", ms*1000)
	}
	if ms < 1000 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}
