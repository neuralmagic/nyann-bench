package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/mockserver"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:            addr,
		TTFT:            1 * time.Millisecond,
		ITL:             1 * time.Millisecond,
		OutputTokens:    5,
		Model:           "test-model",
		ResponseContent: "The answer is 42. #### 42",
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

// TestAutoSetMaxRequestsFromDatasetLen verifies that runScenario auto-sets
// MaxRequests from the dataset's Len() when the config omits max_requests.
// This is the exact bug path: `generate --config` with a gsm8k workload
// and no max_requests would cycle through the dataset indefinitely.
func TestAutoSetMaxRequestsFromDatasetLen(t *testing.T) {
	addr := startTestServer(t)

	// Create a small GSM8K dataset with 5 items
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

	if ds.Len() != 5 {
		t.Fatalf("expected 5 items, got %d", ds.Len())
	}

	// Config mirrors what the deploy Justfile produces: no max_requests set.
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
			MaxRequests: 0, // NOT SET — auto-set should kick in
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

	// Without auto-set, MaxRequests=0 means unlimited — the 30s stage would
	// cycle through all 5 items many times. With auto-set, it stops at 5.
	if summary.TotalRequests != 5 {
		t.Fatalf("expected exactly 5 requests (dataset length), got %d", summary.TotalRequests)
	}
}
