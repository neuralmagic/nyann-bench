package analysis_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

func writeTestRecords(t *testing.T, dir string, records []recorder.Record) {
	t.Helper()
	path := filepath.Join(dir, "requests_0.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestTimestamps(t *testing.T, dir string, ts recorder.Timestamps) {
	t.Helper()
	if err := ts.Write(filepath.Join(dir, "timestamps_0.json")); err != nil {
		t.Fatal(err)
	}
}

func TestComputeBasic(t *testing.T) {
	records := []recorder.Record{
		{RequestID: "r1", ConversationID: "c1", Turn: 0, StartTime: 100.0, EndTime: 100.5,
			TTFT: 50.0, ITLs: []float64{10.0, 12.0, 11.0}, TotalLatencyMs: 500.0,
			OutputTokens: 100, PromptTokens: 50, Status: "ok"},
		{RequestID: "r2", ConversationID: "c1", Turn: 1, StartTime: 100.6, EndTime: 101.2,
			TTFT: 40.0, ITLs: []float64{9.0, 11.0, 10.0}, TotalLatencyMs: 600.0,
			OutputTokens: 120, PromptTokens: 80, Status: "ok"},
		{RequestID: "r3", ConversationID: "c2", Turn: 0, StartTime: 100.1, EndTime: 100.8,
			TTFT: 60.0, ITLs: []float64{8.0, 13.0}, TotalLatencyMs: 700.0,
			OutputTokens: 80, PromptTokens: 40, Status: "ok"},
		{RequestID: "r4", ConversationID: "c3", Turn: 0, StartTime: 100.2, EndTime: 100.3,
			Status: "error", Error: "timeout"},
	}

	s := analysis.Compute(records, 0, 0)

	if s.TotalRequests != 4 {
		t.Errorf("expected 4 total, got %d", s.TotalRequests)
	}
	if s.SuccessRequests != 3 {
		t.Errorf("expected 3 ok, got %d", s.SuccessRequests)
	}
	if s.ErrorRequests != 1 {
		t.Errorf("expected 1 error, got %d", s.ErrorRequests)
	}
	if s.TotalOutputTokens != 300 {
		t.Errorf("expected 300 output tokens, got %d", s.TotalOutputTokens)
	}
	if s.Conversations != 3 {
		t.Errorf("expected 3 conversations, got %d", s.Conversations)
	}

	// TTFT: 40, 50, 60 → mean=50, p50=50
	if s.TTFTMs.Mean != 50.0 {
		t.Errorf("expected TTFT mean 50.0, got %.1f", s.TTFTMs.Mean)
	}
	if s.TTFTMs.Min != 40.0 || s.TTFTMs.Max != 60.0 {
		t.Errorf("TTFT min/max wrong: %.1f/%.1f", s.TTFTMs.Min, s.TTFTMs.Max)
	}
}

func TestComputeWithWindow(t *testing.T) {
	records := []recorder.Record{
		{RequestID: "early", StartTime: 100.0, EndTime: 100.5, TTFT: 50.0, Status: "ok",
			ConversationID: "c1", OutputTokens: 10},
		{RequestID: "in-window", StartTime: 110.0, EndTime: 110.5, TTFT: 30.0, Status: "ok",
			ConversationID: "c2", OutputTokens: 20},
		{RequestID: "late", StartTime: 120.0, EndTime: 121.0, TTFT: 70.0, Status: "ok",
			ConversationID: "c3", OutputTokens: 30},
	}

	// Only "in-window" should be included
	s := analysis.Compute(records, 109.0, 111.0)
	if s.SuccessRequests != 1 {
		t.Errorf("expected 1 request in window, got %d", s.SuccessRequests)
	}
	if s.TTFTMs.Mean != 30.0 {
		t.Errorf("expected TTFT mean 30.0, got %.1f", s.TTFTMs.Mean)
	}
}

func TestLoadRecordsAndTimestamps(t *testing.T) {
	dir := t.TempDir()

	records := []recorder.Record{
		{RequestID: "r1", ConversationID: "c1", StartTime: 100.0, EndTime: 100.5,
			TTFT: 50.0, Status: "ok", OutputTokens: 10},
	}
	writeTestRecords(t, dir, records)
	writeTestTimestamps(t, dir, recorder.Timestamps{
		StartTime: 90.0, RampupEndTime: 95.0, EndTime: 200.0,
		RampupSeconds: 5.0, TotalSeconds: 110.0,
	})

	loaded, err := analysis.LoadRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 record, got %d", len(loaded))
	}

	start, end, err := analysis.LoadTimestamps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if start != 95.0 {
		t.Errorf("expected start=95.0 (rampup end), got %f", start)
	}
	if end != 200.0 {
		t.Errorf("expected end=200.0, got %f", end)
	}
}

func TestComputeStages(t *testing.T) {
	records := []recorder.Record{
		// Stage 0 (c=10, 100-110s)
		{RequestID: "s0-r1", ConversationID: "c1", StartTime: 101.0, EndTime: 103.0,
			TTFT: 50.0, ITLs: []float64{10.0}, TotalLatencyMs: 2000, OutputTokens: 100, Status: "ok"},
		{RequestID: "s0-r2", ConversationID: "c2", StartTime: 102.0, EndTime: 105.0,
			TTFT: 40.0, ITLs: []float64{8.0}, TotalLatencyMs: 3000, OutputTokens: 150, Status: "ok"},
		// Stage 1 (c=20, 110-120s)
		{RequestID: "s1-r1", ConversationID: "c3", StartTime: 111.0, EndTime: 114.0,
			TTFT: 30.0, ITLs: []float64{5.0}, TotalLatencyMs: 3000, OutputTokens: 200, Status: "ok"},
		{RequestID: "s1-r2", ConversationID: "c4", StartTime: 112.0, EndTime: 115.0,
			TTFT: 35.0, ITLs: []float64{6.0}, TotalLatencyMs: 3000, OutputTokens: 180, Status: "ok"},
		{RequestID: "s1-err", ConversationID: "c5", StartTime: 113.0, EndTime: 114.5,
			Status: "error", Error: "timeout"},
	}

	stages := []recorder.StageTimestamp{
		{Stage: 0, Concurrency: 10, StartTime: 100.0, EndTime: 110.0},
		{Stage: 1, Concurrency: 20, StartTime: 110.0, EndTime: 120.0},
	}

	result := analysis.ComputeStages(records, stages)
	if len(result) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(result))
	}

	// Stage 0
	if result[0].Concurrency != 10 {
		t.Errorf("stage 0: expected concurrency 10, got %d", result[0].Concurrency)
	}
	if result[0].Requests != 2 {
		t.Errorf("stage 0: expected 2 requests, got %d", result[0].Requests)
	}
	if result[0].TTFTMs.Mean != 45.0 {
		t.Errorf("stage 0: expected TTFT mean 45.0, got %.1f", result[0].TTFTMs.Mean)
	}

	// Stage 1
	if result[1].Concurrency != 20 {
		t.Errorf("stage 1: expected concurrency 20, got %d", result[1].Concurrency)
	}
	if result[1].Requests != 3 {
		t.Errorf("stage 1: expected 3 requests, got %d", result[1].Requests)
	}
	if result[1].ErrorRate != 1.0/3.0 {
		t.Errorf("stage 1: expected error rate 0.333, got %.3f", result[1].ErrorRate)
	}
}

func TestComputeStagesEmpty(t *testing.T) {
	result := analysis.ComputeStages(nil, nil)
	if result != nil {
		t.Errorf("expected nil for empty stages, got %v", result)
	}
}

func TestFormatSummary(t *testing.T) {
	s := &analysis.Summary{
		TotalRequests:   100,
		SuccessRequests: 95,
		ErrorRequests:   5,
		TotalDurationS:  60.0,
		RequestsPerSec:  1.58,
		TTFTMs:          analysis.LatencyStats{Mean: 50.0, P50: 45.0, P90: 80.0, P99: 120.0, Min: 10.0, Max: 200.0},
	}
	out := analysis.FormatSummary(s)
	if len(out) == 0 {
		t.Fatal("empty summary")
	}
}
