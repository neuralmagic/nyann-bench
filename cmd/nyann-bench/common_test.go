package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":10}`)
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"text\":\"The answer is 42. #### 42\",\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":5,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// TestFiniteDatasetRunsFullDuration verifies that a finite eval dataset
// with MaxRequests=0 runs for the full stage duration instead of stopping
// after exhausting the dataset. This is the regression from PR #47 where
// auto-set MaxRequests caused multi-stage benchmarks to stop early.
func TestFiniteDatasetRunsFullDuration(t *testing.T) {
	addr := startTestServer(t)

	dir := t.TempDir()
	testPath := filepath.Join(dir, "gsm8k_test.jsonl")
	items := `{"question":"What is 1+1?","answer":"1+1=2\n#### 2"}
{"question":"What is 2+2?","answer":"2+2=4\n#### 4"}
{"question":"What is 3+3?","answer":"3+3=6\n#### 6"}
`
	if err := os.WriteFile(testPath, []byte(items), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(testPath, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	sc := &config.ScenarioConfig{
		Target: "http://" + addr + "/v1",
		Model:  "test-model",
		Workload: config.Workload{
			Type:      "gsm8k",
			GSM8KPath: testPath,
		},
		Stages: []config.ScenarioStage{{
			Name:        "bench-stage",
			Duration:    2 * time.Second,
			Mode:        "concurrent",
			Concurrency: 4,
			MaxRequests: 0, // unlimited — should run for full duration
		}},
	}

	var summaryRequests int
	// Race-enabled package tests can briefly starve this process while other
	// packages compile and run. Keep the normal attempt fast, then retry once
	// with a generous bounded window without weakening the wraparound check.
	for attempt, duration := range []time.Duration{2 * time.Second, 10 * time.Second} {
		sc.Stages[0].Duration = duration
		ctx, cancel := context.WithCancel(context.Background())
		summary, err := runScenario(ctx, cancel, scenarioOpts{
			Target:   "http://" + addr + "/v1",
			Model:    "test-model",
			Scenario: sc,
			Dataset:  ds,
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		summaryRequests = summary.TotalRequests
		if summaryRequests > 3 {
			break
		}
		if attempt == 0 {
			t.Logf("only %d requests completed in %s under contention; retrying with %s", summaryRequests, duration, 10*time.Second)
		}
	}

	// With 3 items and 4 concurrent streams in a timed unlimited stage, the
	// dataset must wrap and produce more requests than its finite length.
	if summaryRequests <= 3 {
		t.Fatalf("expected more than 3 requests (dataset should wrap around), got %d", summaryRequests)
	}
}

// TestExplicitMaxRequestsStopsEarly verifies that setting MaxRequests
// explicitly on a stage stops it after the configured number of requests.
func TestExplicitMaxRequestsStopsEarly(t *testing.T) {
	addr := startTestServer(t)

	dir := t.TempDir()
	testPath := filepath.Join(dir, "gsm8k_test.jsonl")
	items := `{"question":"What is 1+1?","answer":"1+1=2\n#### 2"}
{"question":"What is 2+2?","answer":"2+2=4\n#### 4"}
{"question":"What is 3+3?","answer":"3+3=6\n#### 6"}
{"question":"What is 4+4?","answer":"4+4=8\n#### 8"}
{"question":"What is 5+5?","answer":"5+5=10\n#### 10"}
`
	if err := os.WriteFile(testPath, []byte(items), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(testPath, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	sc := &config.ScenarioConfig{
		Target: "http://" + addr + "/v1",
		Model:  "test-model",
		Workload: config.Workload{
			Type:      "gsm8k",
			GSM8KPath: testPath,
		},
		Stages: []config.ScenarioStage{{
			Name:        "gsm8k-eval",
			Duration:    30 * time.Second,
			Mode:        "concurrent",
			Concurrency: 16,
			MaxRequests: 5,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	summary, err := runScenario(ctx, cancel, scenarioOpts{
		Target:   "http://" + addr + "/v1",
		Model:    "test-model",
		Scenario: sc,
		Dataset:  ds,
	})
	if err != nil {
		t.Fatal(err)
	}

	if summary.TotalRequests != 5 {
		t.Fatalf("expected exactly 5 requests, got %d", summary.TotalRequests)
	}
}

// tokenizeHandler answers /tokenize with a word count, recording each model.
func tokenizeHandler(t *testing.T) (http.HandlerFunc, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var models []string
	h := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, body.Model)
		mu.Unlock()
		fmt.Fprintf(w, `{"count":%d}`, max(1, len(strings.Fields(body.Prompt))))
	}
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), models...)
	}
}

func tokenizeServer(t *testing.T) (string, func() []string) {
	t.Helper()
	h, models := tokenizeHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, models
}

func TestTokenizerTargetKeepsTokenizationOffTheInferenceTarget(t *testing.T) {
	var targetTokenizeCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"adapter-3"}]}`)
		case "/tokenize":
			targetTokenizeCalls.Add(1)
			http.Error(w, "tokenize must not reach the inference target", http.StatusTeapot)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(target.Close)
	tokURL, tokModels := tokenizeServer(t)

	sc := &config.ScenarioConfig{
		Workload: config.Workload{Type: "synthetic", ISL: 64, OSL: 4, Turns: 2},
		Stages:   []config.ScenarioStage{{Duration: 300 * time.Millisecond, Mode: "concurrent", Concurrency: 1}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	summary, err := runScenario(ctx, cancel, scenarioOpts{
		Target:          target.URL + "/v1",
		Model:           "adapter-3",
		Scenario:        sc,
		TokenizerTarget: tokURL + "/v1",
		TokenizerModel:  "base-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalRequests == 0 {
		t.Fatal("no requests ran")
	}
	if n := targetTokenizeCalls.Load(); n != 0 {
		t.Fatalf("inference target received %d /tokenize calls", n)
	}
	models := tokModels()
	if len(models) == 0 {
		t.Fatal("tokenizer target received no /tokenize calls")
	}
	for _, m := range models {
		if m != "base-model" {
			t.Fatalf("tokenizer call used model %q, want base-model", m)
		}
	}
}

func TestTokenizationDefaultsToTargetAndModel(t *testing.T) {
	tokenize, tokModels := tokenizeHandler(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"adapter-3"}]}`)
		case "/tokenize":
			tokenize(w, r)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(target.Close)

	sc := &config.ScenarioConfig{
		Workload: config.Workload{Type: "synthetic", ISL: 64, OSL: 4, Turns: 1},
		Stages:   []config.ScenarioStage{{Duration: 200 * time.Millisecond, Mode: "concurrent", Concurrency: 1}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := runScenario(ctx, cancel, scenarioOpts{
		Target:   target.URL + "/v1",
		Model:    "adapter-3",
		Scenario: sc,
	}); err != nil {
		t.Fatal(err)
	}
	models := tokModels()
	if len(models) == 0 {
		t.Fatal("no /tokenize calls reached the target")
	}
	for _, m := range models {
		if m != "adapter-3" {
			t.Fatalf("tokenizer call used model %q, want the request model adapter-3", m)
		}
	}
}
