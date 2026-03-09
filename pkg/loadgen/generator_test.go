package loadgen_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/mockserver"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
)

func startMockServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:         addr,
		TTFT:         5 * time.Millisecond,
		ITL:          1 * time.Millisecond,
		OutputTokens: 10,
		Model:        "test-model",
	}
	go srv.ListenAndServe()

	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return ""
}

func TestGeneratorBasic(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 2,
		Rampup:      0,
		Duration:    500 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(32, 10, 1, 4.0),
		Recorder:    rec,
	}

	ts, err := gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	if ts.StartTime == 0 || ts.EndTime == 0 {
		t.Fatal("timestamps should be non-zero")
	}
	if ts.TotalSeconds < 0.4 || ts.TotalSeconds > 2.0 {
		t.Errorf("unexpected total duration: %f", ts.TotalSeconds)
	}

	// Check that records were written
	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected at least one record")
	}

	okCount := 0
	for _, r := range records {
		if r.Status == "ok" {
			okCount++
			if r.TTFT <= 0 {
				t.Errorf("record %s has zero TTFT", r.RequestID)
			}
		}
	}
	if okCount == 0 {
		t.Fatal("expected at least one successful record")
	}
}

func TestGeneratorMultiTurn(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    1 * time.Second,
		Dataset:     dataset.NewSynthetic(32, 10, 3, 4.0), // 3 turns
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected records")
	}

	// Check that we have multi-turn conversations (turns 0, 1, 2)
	turnsSeen := map[int]bool{}
	for _, r := range records {
		turnsSeen[r.Turn] = true
	}
	if !turnsSeen[0] || !turnsSeen[1] || !turnsSeen[2] {
		t.Errorf("expected turns 0, 1, 2; got turns: %v", turnsSeen)
	}
}

func TestGeneratorRampup(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 4,
		Rampup:      200 * time.Millisecond,
		Duration:    800 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(16, 5, 1, 4.0),
		Recorder:    rec,
	}

	ts, err := gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	// Rampup should be 200ms
	if ts.RampupSeconds < 0.15 || ts.RampupSeconds > 0.3 {
		t.Errorf("unexpected rampup duration: %f", ts.RampupSeconds)
	}

	// Check that first requests from different streams are staggered
	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	// Find first request per stream
	firstByStream := map[int]float64{}
	for _, r := range records {
		if _, ok := firstByStream[r.StreamID]; !ok {
			firstByStream[r.StreamID] = r.StartTime
		}
	}

	if len(firstByStream) < 2 {
		t.Skip("not enough streams completed to verify stagger")
	}

	// Streams should start at different times
	var minT, maxT float64
	first := true
	for _, st := range firstByStream {
		if first || st < minT {
			minT = st
		}
		if first || st > maxT {
			maxT = st
		}
		first = false
	}
	spread := maxT - minT
	if spread < 0.05 { // At least 50ms spread with 200ms rampup
		t.Errorf("streams not staggered enough: spread=%.1fms", spread*1000)
	}
}

func TestTimestampsWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timestamps.json")

	ts := &recorder.Timestamps{
		StartTime:     1000.0,
		RampupEndTime: 1060.0,
		EndTime:       1660.0,
		RampupSeconds: 60.0,
		TotalSeconds:  660.0,
	}

	if err := ts.Write(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var loaded recorder.Timestamps
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if loaded.StartTime != 1000.0 || loaded.RampupEndTime != 1060.0 || loaded.EndTime != 1660.0 {
		t.Errorf("timestamps mismatch: %+v", loaded)
	}
}

func startMockServerWithContent(t *testing.T, content string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:            addr,
		TTFT:            5 * time.Millisecond,
		ITL:             1 * time.Millisecond,
		OutputTokens:    10,
		Model:           "test-model",
		ResponseContent: content,
	}
	go srv.ListenAndServe()

	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return ""
}

func TestGeneratorEvalCorrect(t *testing.T) {
	// Mock server returns a response containing "#### 42"
	addr := startMockServerWithContent(t, "Let me solve this step by step.\n3 * 14 = 42\n#### 42")
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	// Create a dataset that sets ExpectedAnswer
	ds := &evalDataset{
		answer: "42",
	}

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    2 * time.Second,
		Dataset:     ds,
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected at least one record")
	}

	for _, r := range records {
		if r.Status != "ok" {
			continue
		}
		if r.EvalCorrect == nil {
			t.Fatal("expected EvalCorrect to be set")
		}
		if !*r.EvalCorrect {
			t.Errorf("expected correct eval: expected=%q extracted=%q", r.EvalExpected, r.EvalExtracted)
		}
		if r.EvalExpected != "42" {
			t.Errorf("expected EvalExpected=42, got %q", r.EvalExpected)
		}
		if r.EvalExtracted != "42" {
			t.Errorf("expected EvalExtracted=42, got %q", r.EvalExtracted)
		}
	}
}

func TestGeneratorEvalIncorrect(t *testing.T) {
	// Mock server returns "#### 99" but expected answer is "42"
	addr := startMockServerWithContent(t, "The answer is #### 99")
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ds := &evalDataset{answer: "42"}

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    2 * time.Second,
		Dataset:     ds,
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	found := false
	for _, r := range records {
		if r.Status != "ok" || r.EvalCorrect == nil {
			continue
		}
		found = true
		if *r.EvalCorrect {
			t.Error("expected incorrect eval")
		}
		if r.EvalExtracted != "99" {
			t.Errorf("expected EvalExtracted=99, got %q", r.EvalExtracted)
		}
	}
	if !found {
		t.Fatal("no eval records found")
	}
}

func TestGeneratorEvalIncorrectWhenNoAnswer(t *testing.T) {
	// Mock server returns default "tok tok tok..." — no number to extract, scored as incorrect
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ds := &evalDataset{answer: "42"}

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    2 * time.Second,
		Dataset:     ds,
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	found := false
	for _, r := range records {
		if r.Status != "ok" || r.EvalCorrect == nil {
			continue
		}
		found = true
		if *r.EvalCorrect {
			t.Error("expected incorrect eval (no answer extractable)")
		}
	}
	if !found {
		t.Fatal("no eval records found")
	}
}

func TestGeneratorCompletionsEval(t *testing.T) {
	// Test that the completions API path works with eval
	addr := startMockServerWithContent(t, "Let me solve this step by step.\n6 * 7 = 42\n#### 42")
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ds := &completionEvalDataset{answer: "42"}

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    2 * time.Second,
		Dataset:     ds,
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	found := false
	for _, r := range records {
		if r.Status != "ok" || r.EvalCorrect == nil {
			continue
		}
		found = true
		if !*r.EvalCorrect {
			t.Error("expected correct eval")
		}
		if r.EvalExtracted != "42" {
			t.Errorf("expected EvalExtracted='42', got %q", r.EvalExtracted)
		}
	}
	if !found {
		t.Fatal("no eval records found")
	}
}

// evalDataset is a test dataset that returns single-turn conversations with ExpectedAnswer.
type evalDataset struct {
	answer string
}

func (d *evalDataset) NextConversation() dataset.Conversation {
	return dataset.Conversation{
		Turns: [][]client.Message{
			{
				{Role: "user", Content: "What is 6 * 7?"},
			},
		},
		MaxTokens:      100,
		ExpectedAnswer: d.answer,
	}
}

// completionEvalDataset returns conversations that use the completions API path.
type completionEvalDataset struct {
	answer string
}

func (d *completionEvalDataset) NextConversation() dataset.Conversation {
	return dataset.Conversation{
		Prompt:         "Question: What is 6 * 7?\nAnswer:",
		ExpectedAnswer: d.answer,
	}
}

func TestRunStagesPoolResize(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:   "http://" + addr + "/v1",
		Model:    "test-model",
		Dataset:  dataset.NewSynthetic(32, 10, 1, 4.0),
		Recorder: rec,
	}

	stages := []loadgen.Stage{
		{Concurrency: 2, Duration: 500 * time.Millisecond},
		{Concurrency: 8, Duration: 500 * time.Millisecond},
		{Concurrency: 4, Duration: 500 * time.Millisecond},
	}

	var stageLog []int
	gen.RunStages(context.Background(), stages, func(i, concurrency int) {
		stageLog = append(stageLog, concurrency)
	})

	if len(stageLog) != 3 {
		t.Fatalf("expected 3 stage callbacks, got %d", len(stageLog))
	}
	if stageLog[0] != 2 || stageLog[1] != 8 || stageLog[2] != 4 {
		t.Errorf("unexpected stage concurrencies: %v", stageLog)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected records from pool-based stages")
	}

	// Verify no large gaps between requests (pool should not tear down between stages).
	// With the mock server (5ms TTFT + 10*1ms ITL = ~15ms per request), a gap > 200ms
	// would indicate the pool was torn down and restarted.
	var maxGap float64
	for i := 1; i < len(records); i++ {
		gap := records[i].StartTime - records[i-1].EndTime
		if gap > maxGap {
			maxGap = gap
		}
	}
	// With multiple concurrent streams, gaps should be minimal.
	// A torn-down pool would show gaps of 100ms+ as all streams finish then restart.
	if maxGap > 0.2 {
		t.Errorf("max gap between requests was %.3fs, expected < 0.2s (pool may have torn down between stages)", maxGap)
	}
}

func TestGeneratorGPQAEval(t *testing.T) {
	// Mock server returns a CoT response with "The answer is (B)"
	addr := startMockServerWithContent(t, "Let me think step by step. First, mitochondria are known as the powerhouse of the cell. The answer is (B).")
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	ds := &mcEvalDataset{answer: "(B)"}

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    2 * time.Second,
		Dataset:     ds,
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	found := false
	for _, r := range records {
		if r.Status != "ok" || r.EvalCorrect == nil {
			continue
		}
		found = true
		if !*r.EvalCorrect {
			t.Errorf("expected correct eval: expected=%q extracted=%q", r.EvalExpected, r.EvalExtracted)
		}
		if r.EvalExtracted != "B" {
			t.Errorf("expected EvalExtracted='B', got %q", r.EvalExtracted)
		}
	}
	if !found {
		t.Fatal("no eval records found")
	}
}

// mcEvalDataset returns completions-style conversations with multiple-choice ExpectedAnswer.
type mcEvalDataset struct {
	answer string
}

func (d *mcEvalDataset) NextConversation() dataset.Conversation {
	greedy := 0.0
	return dataset.Conversation{
		Prompt:         "What is the correct answer?\nChoices:\n(A) Wrong\n(B) Right\n(C) Wrong\n(D) Wrong\nLet's think step by step: ",
		MaxTokens:      256,
		Temperature:    &greedy,
		ExpectedAnswer: d.answer,
	}
}

func readRecords(t *testing.T, path string) []recorder.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var records []recorder.Record
	dec := json.NewDecoder(strings.NewReader(string(data)))
	for dec.More() {
		var r recorder.Record
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}
