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
// and optionally enriched with Prometheus metrics.
type StageSummary struct {
	Concurrency        int          `json:"concurrency"`
	TotalRequests      int          `json:"total_requests"`
	SuccessRequests    int          `json:"successful_requests"`
	ErrorRequests      int          `json:"error_requests"`
	DurationS          float64      `json:"duration_seconds"`
	TotalOutputTokens  int          `json:"total_output_tokens"`
	TotalPromptTokens  int          `json:"total_prompt_tokens"`
	OutputTokensPerS   float64      `json:"output_tokens_per_second"`
	TTFTMs             LatencyStats `json:"ttft_ms"`
	ITLMs              LatencyStats `json:"itl_ms"`
	E2EMs              LatencyStats `json:"e2e_latency_ms"`
	InterTurnWait      LatencyStats `json:"inter_turn_wait_ms,omitempty"`

	// Server-side metrics from Prometheus (populated by QueryStageServerMetrics).
	Server *ServerMetrics `json:"server,omitempty"`
}

// ServerMetrics holds server-side metrics queried from Prometheus.
type ServerMetrics struct {
	Aggregate       bool                   `json:"aggregate,omitempty"`
	TTFT            prometheus.LatencyStats `json:"ttft_seconds"`
	ITL             prometheus.LatencyStats `json:"itl_seconds"`
	OutputTokensP50 float64                `json:"output_tokens_per_second_p50"`
	OutputTokensMax float64                `json:"output_tokens_per_second_max"`
	PrefillKVMin    float64                `json:"prefill_kv_min"`
	PrefillKVMax    float64                `json:"prefill_kv_max"`
	DecodeKVMin     float64                `json:"decode_kv_min"`
	DecodeKVMax     float64                `json:"decode_kv_max"`
}

// HasData returns true if any metric field is non-zero.
func (s *ServerMetrics) HasData() bool {
	if s == nil {
		return false
	}
	return s.TTFT.P50 != 0 || s.ITL.P50 != 0 || s.OutputTokensP50 != 0 || s.OutputTokensMax != 0
}

// ComputePerStage computes client-side statistics for each stage from records.
func ComputePerStage(records []recorder.Record, stages []recorder.StageTimestamp) []StageSummary {
	var summaries []StageSummary
	for _, stage := range stages {
		var ttfts, e2es, allITLs, interTurnWaits []float64
		totalOK, totalErr, totalOutTok, totalPromptTok := 0, 0, 0, 0
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
				totalPromptTok += r.PromptTokens
			} else {
				totalErr++
			}
			if r.InterTurnWaitMs > 0 {
				interTurnWaits = append(interTurnWaits, r.InterTurnWaitMs)
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
			TotalPromptTokens: totalPromptTok,
			OutputTokensPerS:  tokPerSec,
			TTFTMs:            computeLatencyStats(ttfts),
			ITLMs:             computeLatencyStats(allITLs),
			E2EMs:             computeLatencyStats(e2es),
			InterTurnWait:     computeLatencyStats(interTurnWaits),
		})
	}
	return summaries
}

func trimStage(start, end time.Time) (time.Time, time.Time) {
	dur := end.Sub(start)
	return start.Add(time.Duration(float64(dur) * 0.20)),
		start.Add(time.Duration(float64(dur) * 0.95))
}

// QueryStageServerMetrics queries Prometheus for server-side vLLM metrics for a single stage.
// It auto-detects aggregate vs PD mode from the deploy name: if it contains "-decode",
// PD mode is assumed (separate prefill/decode KV queries); otherwise aggregate mode.
// All queries include a job label filter to avoid cross-deployment interference.
func QueryStageServerMetrics(client *prometheus.Client, ts recorder.StageTimestamp, deployName string) *ServerMetrics {
	podFilter := deployName + ".*"
	isAggregate := !strings.Contains(deployName, "-decode")

	start := floatToTime(ts.StartTime)
	end := floatToTime(ts.EndTime)

	trimStart, trimEnd := trimStage(start, end)
	if trimEnd.Sub(trimStart) < 5*time.Second {
		trimStart, trimEnd = start, end
	}

	sm := &ServerMetrics{Aggregate: isAggregate}

	jobFilter := "vllm-aggregate"
	if !isAggregate {
		jobFilter = "vllm-decode"
	}

	kvQueries := 1
	if !isAggregate {
		kvQueries = 2
	}
	var wg sync.WaitGroup
	wg.Add(3 + kvQueries)

	go func() {
		defer wg.Done()
		stats, err := client.HistogramQuantile(
			"vllm:time_to_first_token_seconds_bucket", podFilter, jobFilter, trimStart, trimEnd)
		if err != nil {
			slog.Debug("Failed to query server TTFT", "error", err)
			return
		}
		sm.TTFT = stats
	}()

	go func() {
		defer wg.Done()
		stats, err := client.HistogramQuantile(
			"vllm:inter_token_latency_seconds_bucket", podFilter, jobFilter, trimStart, trimEnd)
		if err != nil {
			slog.Debug("Failed to query server ITL", "error", err)
			return
		}
		sm.ITL = stats
	}()

	go func() {
		defer wg.Done()
		q := fmt.Sprintf(`sum(rate(vllm:generation_tokens_total{job="%s", pod=~"%s"}[10s]))`, jobFilter, podFilter)
		stats, err := client.QueryGaugeStats(q, trimStart, trimEnd)
		if err != nil {
			slog.Debug("Failed to query server TOK/s", "error", err)
			return
		}
		sm.OutputTokensP50 = stats.P50
		sm.OutputTokensMax = stats.Max
	}()

	if isAggregate {
		go func() {
			defer wg.Done()
			minQ := fmt.Sprintf(`min(vllm:kv_cache_usage_perc{job="%s", pod=~"%s"})`, jobFilter, podFilter)
			minStats, err := client.QueryGaugeStats(minQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query KV$ min", "error", err)
				return
			}
			maxQ := fmt.Sprintf(`max(vllm:kv_cache_usage_perc{job="%s", pod=~"%s"})`, jobFilter, podFilter)
			maxStats, err := client.QueryGaugeStats(maxQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query KV$ max", "error", err)
				return
			}
			sm.DecodeKVMin = minStats.Min
			sm.DecodeKVMax = maxStats.Max
		}()
	} else {
		prefillPodFilter := strings.Replace(deployName, "-decode", "-prefill", 1) + ".*"

		go func() {
			defer wg.Done()
			minQ := fmt.Sprintf(`min(vllm:kv_cache_usage_perc{job="vllm-prefill", pod=~"%s"})`, prefillPodFilter)
			minStats, err := client.QueryGaugeStats(minQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query prefill KV$ min", "error", err)
				return
			}
			maxQ := fmt.Sprintf(`max(vllm:kv_cache_usage_perc{job="vllm-prefill", pod=~"%s"})`, prefillPodFilter)
			maxStats, err := client.QueryGaugeStats(maxQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query prefill KV$ max", "error", err)
				return
			}
			sm.PrefillKVMin = minStats.Min
			sm.PrefillKVMax = maxStats.Max
		}()

		go func() {
			defer wg.Done()
			minQ := fmt.Sprintf(`min(vllm:kv_cache_usage_perc{job="vllm-decode", pod=~"%s"})`, podFilter)
			minStats, err := client.QueryGaugeStats(minQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query decode KV$ min", "error", err)
				return
			}
			maxQ := fmt.Sprintf(`max(vllm:kv_cache_usage_perc{job="vllm-decode", pod=~"%s"})`, podFilter)
			maxStats, err := client.QueryGaugeStats(maxQ, trimStart, trimEnd)
			if err != nil {
				slog.Debug("Failed to query decode KV$ max", "error", err)
				return
			}
			sm.DecodeKVMin = minStats.Min
			sm.DecodeKVMax = maxStats.Max
		}()
	}

	wg.Wait()
	return sm
}

// FormatStageHeader returns the table header line. Pass a non-nil ServerMetrics
// to include server-side columns; its Aggregate field controls KV column layout.
func FormatStageHeader(sm *ServerMetrics) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%6s  %6s  %5s  %9s  %9s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s",
		"CONC", "OK", "ERR", "TOT_TOK", "TOK/S",
		"TTFT_AVG", "TTFT_P10", "TTFT_P50", "TTFT_P95", "TTFT_P99",
		"ITL_P10", "ITL_P50", "ITL_P95", "ITL_P99",
		"ITW_P50", "ITW_P99")
	width := 180
	if sm != nil {
		fmt.Fprintf(&b, "  %9s  %10s  %10s  %10s  %10s  %10s  %10s",
			"SRV_TOK/S", "SRV_TTFT50", "SRV_TTFT99", "SRV_ITL10", "SRV_ITL50", "SRV_ITL95", "SRV_ITL99")
		if sm.Aggregate {
			fmt.Fprintf(&b, "  %7s  %7s", "KV_MIN", "KV_MAX")
			width = 278
		} else {
			fmt.Fprintf(&b, "  %7s  %7s  %7s  %7s", "PKV_MIN", "PKV_MAX", "DKV_MIN", "DKV_MAX")
			width = 294
		}
	}
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("-", width))
	b.WriteByte('\n')
	return b.String()
}

// FormatStageRow returns a single table row for a stage.
func FormatStageRow(s StageSummary) string {
	row := fmt.Sprintf("%6d  %6d  %5d  %9d  %9.1f  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s  %10s",
		s.Concurrency, s.SuccessRequests, s.ErrorRequests,
		s.TotalOutputTokens, s.OutputTokensPerS,
		fmtMs(s.TTFTMs.Mean), fmtMs(s.TTFTMs.P10), fmtMs(s.TTFTMs.P50), fmtMs(s.TTFTMs.P95), fmtMs(s.TTFTMs.P99),
		fmtMs(s.ITLMs.P10), fmtMs(s.ITLMs.P50), fmtMs(s.ITLMs.P95), fmtMs(s.ITLMs.P99),
		fmtMs(s.InterTurnWait.P50), fmtMs(s.InterTurnWait.P99))
	if s.Server != nil {
		sm := s.Server
		row += fmt.Sprintf("  %9.1f  %10s  %10s  %10s  %10s  %10s  %10s",
			sm.OutputTokensP50,
			fmtDuration(sm.TTFT.P50), fmtDuration(sm.TTFT.P99),
			fmtDuration(sm.ITL.P10), fmtDuration(sm.ITL.P50),
			fmtDuration(sm.ITL.P95), fmtDuration(sm.ITL.P99))
		if sm.Aggregate {
			row += fmt.Sprintf("  %7s  %7s",
				fmtPct(sm.DecodeKVMin), fmtPct(sm.DecodeKVMax))
		} else {
			row += fmt.Sprintf("  %7s  %7s  %7s  %7s",
				fmtPct(sm.PrefillKVMin), fmtPct(sm.PrefillKVMax),
				fmtPct(sm.DecodeKVMin), fmtPct(sm.DecodeKVMax))
		}
	}
	return row + "\n"
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

func fmtPct(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

func floatToTime(f float64) time.Time {
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}
