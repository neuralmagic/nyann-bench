package analysis_test

import (
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

func TestComputePerStage(t *testing.T) {
	records := []recorder.Record{
		{RequestID: "s1-a", ConversationID: "c1", StartTime: 100.0, EndTime: 100.5,
			TTFT: 50, ITLs: []float64{10, 12}, TotalLatencyMs: 500, OutputTokens: 80, Status: "ok"},
		{RequestID: "s1-b", ConversationID: "c2", StartTime: 100.2, EndTime: 100.8,
			TTFT: 40, ITLs: []float64{9, 11, 13}, TotalLatencyMs: 600, OutputTokens: 120, Status: "ok"},
		{RequestID: "s1-err", ConversationID: "c3", StartTime: 100.1, EndTime: 100.3,
			Status: "error"},
		// Spans the stage boundary — should be excluded from both stages.
		{RequestID: "boundary", ConversationID: "c4", StartTime: 100.9, EndTime: 101.2,
			TTFT: 30, ITLs: []float64{8}, TotalLatencyMs: 300, OutputTokens: 50, Status: "ok"},
		{RequestID: "s2-a", ConversationID: "c5", StartTime: 101.0, EndTime: 101.5,
			TTFT: 60, ITLs: []float64{14, 16}, TotalLatencyMs: 500, OutputTokens: 100, Status: "ok"},
	}

	stages := []recorder.StageTimestamp{
		{Stage: 0, Concurrency: 32, StartTime: 100.0, EndTime: 101.0},
		{Stage: 1, Concurrency: 64, StartTime: 101.0, EndTime: 102.0},
	}

	summaries := analysis.ComputePerStage(records, stages)
	if len(summaries) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(summaries))
	}

	s0 := summaries[0]
	if s0.Concurrency != 32 {
		t.Errorf("stage 0: expected concurrency 32, got %d", s0.Concurrency)
	}
	if s0.SuccessRequests != 2 {
		t.Errorf("stage 0: expected 2 ok, got %d", s0.SuccessRequests)
	}
	if s0.ErrorRequests != 1 {
		t.Errorf("stage 0: expected 1 error, got %d", s0.ErrorRequests)
	}
	if s0.TotalOutputTokens != 200 {
		t.Errorf("stage 0: expected 200 output tokens, got %d", s0.TotalOutputTokens)
	}
	// ITLs: 9, 10, 11, 12, 13 → min=9, max=13
	if s0.ITLMs.Min != 9.0 || s0.ITLMs.Max != 13.0 {
		t.Errorf("stage 0: ITL min/max = %.1f/%.1f, want 9/13", s0.ITLMs.Min, s0.ITLMs.Max)
	}

	s1 := summaries[1]
	if s1.Concurrency != 64 {
		t.Errorf("stage 1: expected concurrency 64, got %d", s1.Concurrency)
	}
	if s1.SuccessRequests != 1 {
		t.Errorf("stage 1: expected 1 ok, got %d", s1.SuccessRequests)
	}
	if s1.TotalOutputTokens != 100 {
		t.Errorf("stage 1: expected 100 output tokens, got %d", s1.TotalOutputTokens)
	}
}

func TestComputePerStageEmpty(t *testing.T) {
	summaries := analysis.ComputePerStage(nil, []recorder.StageTimestamp{
		{Stage: 0, Concurrency: 8, StartTime: 100.0, EndTime: 200.0},
	})
	if len(summaries) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(summaries))
	}
	if summaries[0].TotalRequests != 0 {
		t.Errorf("expected 0 requests, got %d", summaries[0].TotalRequests)
	}
}

func TestFormatStageTable(t *testing.T) {
	s := analysis.StageSummary{
		Concurrency:       64,
		SuccessRequests:   100,
		ErrorRequests:     2,
		TotalOutputTokens: 50000,
		OutputTokensPerS:  833.3,
		ITLMs:             analysis.LatencyStats{P10: 10, P50: 25, P95: 40, P99: 50},
	}

	header := analysis.FormatStageHeader(false)
	if len(header) == 0 {
		t.Fatal("empty header")
	}

	row := analysis.FormatStageRow(s)
	if len(row) == 0 {
		t.Fatal("empty row")
	}
}
