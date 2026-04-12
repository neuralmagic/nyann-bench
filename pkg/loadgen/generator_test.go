package loadgen_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/mockserver"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
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

func TestCacheSaltAppearsInRequest(t *testing.T) {
	// Start a tiny HTTP server that captures the request body
	var bodies []string
	var mu sync.Mutex
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		// Return a minimal streaming response so the client doesn't error
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta":         map[string]string{"content": "hi"},
				"finish_reason": "stop",
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	})
	srv := &http.Server{Handler: capture}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	rec := recorder.NewMemory()
	gen := &loadgen.Generator{
		Target:    "http://" + ln.Addr().String() + "/v1",
		Model:     "test-model",
		CacheSalt: &config.CacheSalt{Mode: "fixed", Value: "test-salt-abc"},
		Dataset:   dataset.NewSynthetic(8, 4, 1, 4.0),
		Recorder:  rec,
		Duration:  500 * time.Millisecond,
		Concurrency: 1,
	}

	gen.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("expected at least one captured request")
	}
	for i, b := range bodies {
		if !strings.Contains(b, `"cache_salt":"test-salt-abc"`) {
			t.Errorf("request %d missing cache_salt: %s", i, b)
		}
	}
}

func TestCacheSaltOmittedWhenNil(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta":         map[string]string{"content": "hi"},
				"finish_reason": "stop",
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	})
	srv := &http.Server{Handler: capture}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	rec := recorder.NewMemory()
	gen := &loadgen.Generator{
		Target:      "http://" + ln.Addr().String() + "/v1",
		Model:       "test-model",
		Dataset:     dataset.NewSynthetic(8, 4, 1, 4.0),
		Recorder:    rec,
		Duration:    500 * time.Millisecond,
		Concurrency: 1,
	}

	gen.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("expected at least one captured request")
	}
	for i, b := range bodies {
		if strings.Contains(b, "cache_salt") {
			t.Errorf("request %d should not contain cache_salt: %s", i, b)
		}
	}
}

func TestRandomCacheSaltUnique(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta":         map[string]string{"content": "hi"},
				"finish_reason": "stop",
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	})
	srv := &http.Server{Handler: capture}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	rec := recorder.NewMemory()
	gen := &loadgen.Generator{
		Target:    "http://" + ln.Addr().String() + "/v1",
		Model:     "test-model",
		CacheSalt: &config.CacheSalt{Mode: "random"},
		Dataset:   dataset.NewSynthetic(8, 4, 1, 4.0),
		Recorder:        rec,
		Duration:        1 * time.Second,
		Concurrency:     1,
	}

	gen.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(bodies))
	}

	// Extract cache_salt values and verify they're all unique
	salts := make(map[string]bool)
	for i, b := range bodies {
		var req struct {
			CacheSalt string `json:"cache_salt"`
		}
		if err := json.Unmarshal([]byte(b), &req); err != nil {
			t.Fatalf("request %d: unmarshal error: %v", i, err)
		}
		if req.CacheSalt == "" {
			t.Errorf("request %d: cache_salt is empty", i)
		}
		if salts[req.CacheSalt] {
			t.Errorf("request %d: duplicate cache_salt %q", i, req.CacheSalt)
		}
		salts[req.CacheSalt] = true
	}
}

func TestMultiTurnFeedsRealResponses(t *testing.T) {
	// Capture request bodies to verify that turn N+1 contains the real
	// model response from turn N, not a synthetic placeholder.
	var bodies []string
	var mu sync.Mutex
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta":         map[string]string{"content": "real-model-output"},
				"finish_reason": "stop",
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		f.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	})
	srv := &http.Server{Handler: capture}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	rec := recorder.NewMemory()
	gen := &loadgen.Generator{
		Target:      "http://" + ln.Addr().String() + "/v1",
		Model:       "test-model",
		Dataset:     dataset.NewSynthetic(8, 4, 3, 4.0), // 3 turns
		Recorder:    rec,
		Duration:    500 * time.Millisecond,
		Concurrency: 1,
	}

	gen.Run(context.Background())

	mu.Lock()
	defer mu.Unlock()

	// With 3 turns per conversation, bodies come in groups of 3.
	// Need at least one full conversation.
	if len(bodies) < 3 {
		t.Fatalf("expected at least 3 requests (one conversation), got %d", len(bodies))
	}

	// Parse the messages from each turn of the first conversation.
	type reqBody struct {
		Messages []client.Message `json:"messages"`
	}
	for turn := 0; turn < 3; turn++ {
		var req reqBody
		if err := json.Unmarshal([]byte(bodies[turn]), &req); err != nil {
			t.Fatalf("turn %d: unmarshal: %v", turn, err)
		}

		if turn == 0 {
			// Turn 0: just one user message.
			if len(req.Messages) != 1 {
				t.Errorf("turn 0: expected 1 message, got %d", len(req.Messages))
			}
			continue
		}

		// Turn 1+: should contain real model output, not synthetic placeholder.
		// Expected structure: [user, assistant, user, assistant, ..., user]
		expectedLen := turn*2 + 1
		if len(req.Messages) != expectedLen {
			t.Errorf("turn %d: expected %d messages, got %d", turn, expectedLen, len(req.Messages))
			continue
		}

		// Every assistant message should be the real model output.
		for i, msg := range req.Messages {
			if msg.Role == "assistant" {
				if msg.Content != "real-model-output" {
					t.Errorf("turn %d, message %d: expected real model output, got %q", turn, i, msg.Content)
				}
			}
		}
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
