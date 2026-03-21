package warmup_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/mockserver"
	"github.com/neuralmagic/nyann_poker/pkg/warmup"
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

func TestComputeStages(t *testing.T) {
	addr := startMockServer(t)

	cfg := &warmup.AutoConfig{
		Target:            "http://" + addr + "/v1",
		Model:             "test-model",
		Dataset:           dataset.NewSynthetic(32, 10, 1, 4.0),
		TargetConcurrency: 8,
		WorkloadOSL:       10,
	}

	stages, err := warmup.ComputeStages(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ComputeStages failed: %v", err)
	}

	if len(stages) != 1 {
		t.Fatalf("expected 1 stage (settle), got %d", len(stages))
	}

	// Settle at target with staggered rampup
	if stages[0].Concurrency != 8 {
		t.Errorf("stage 0 concurrency: got %d, want 8", stages[0].Concurrency)
	}
	if stages[0].Rampup <= 0 {
		t.Error("stage 0 rampup should be > 0 (stagger across request lifetime)")
	}
	if stages[0].Duration < 5*time.Second {
		t.Errorf("stage 0 duration should be >= 5s, got %v", stages[0].Duration)
	}
}

func TestComputeStagesConcurrency1(t *testing.T) {
	addr := startMockServer(t)

	cfg := &warmup.AutoConfig{
		Target:            "http://" + addr + "/v1",
		Model:             "test-model",
		Dataset:           dataset.NewSynthetic(32, 10, 1, 4.0),
		TargetConcurrency: 1,
		WorkloadOSL:       10,
	}

	stages, err := warmup.ComputeStages(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ComputeStages failed: %v", err)
	}

	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0].Concurrency != 1 {
		t.Errorf("stage 0: got C=%d, want 1", stages[0].Concurrency)
	}
}
