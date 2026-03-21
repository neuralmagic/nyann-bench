package warmup_test

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
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

func TestRunCharacterize(t *testing.T) {
	addr := startMockServer(t)

	cfg := &warmup.CharacterizeConfig{
		Target:            "http://" + addr + "/v1",
		Model:             "test-model",
		Dataset:           dataset.NewSynthetic(32, 10, 1, 4.0),
		TargetConcurrency: 4,
		WorkloadISL:       32,
		WorkloadOSL:       10,
		RequestsPerStream: 3,
		DiscardPerLevel:   1,
	}

	profile, err := warmup.RunCharacterize(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunCharacterize failed: %v", err)
	}

	// Verify basic profile fields
	if profile.Model != "test-model" {
		t.Errorf("Model: got %q, want test-model", profile.Model)
	}
	if profile.WorkloadISL != 32 {
		t.Errorf("WorkloadISL: got %d, want 32", profile.WorkloadISL)
	}
	if profile.WorkloadOSL != 10 {
		t.Errorf("WorkloadOSL: got %d, want 10", profile.WorkloadOSL)
	}
	if profile.TargetConcurrency != 4 {
		t.Errorf("TargetConcurrency: got %d, want 4", profile.TargetConcurrency)
	}

	// Kernel warmup should have detected stability
	if profile.KernelWarmupRequests <= 0 {
		t.Error("KernelWarmupRequests should be > 0")
	}

	// TTFT should be positive
	if profile.TTFTMs <= 0 {
		t.Errorf("TTFTMs should be > 0, got %f", profile.TTFTMs)
	}

	// Should have measurement levels
	if len(profile.Levels) == 0 {
		t.Fatal("expected at least one measurement level")
	}

	// Levels should include C=1
	if profile.Levels[0].Concurrency != 1 {
		t.Errorf("first level should be C=1, got C=%d", profile.Levels[0].Concurrency)
	}

	// TPOT model should have positive intercept
	if profile.TPOTModel.A <= 0 {
		t.Errorf("TPOT model intercept should be > 0, got %f", profile.TPOTModel.A)
	}

	// Derived metrics
	if profile.Derived.Regime == "" {
		t.Error("Derived.Regime should be set")
	}

	// Model should predict positive TPOT
	predicted := profile.TPOTModel.Predict(4)
	if predicted <= 0 {
		t.Errorf("Predict(4) should be > 0, got %f", predicted)
	}
}

func TestRunCharacterizeConcurrency1(t *testing.T) {
	addr := startMockServer(t)

	cfg := &warmup.CharacterizeConfig{
		Target:            "http://" + addr + "/v1",
		Model:             "test-model",
		Dataset:           dataset.NewSynthetic(32, 10, 1, 4.0),
		TargetConcurrency: 1,
		WorkloadISL:       32,
		WorkloadOSL:       10,
		RequestsPerStream: 3,
		DiscardPerLevel:   1,
	}

	profile, err := warmup.RunCharacterize(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunCharacterize failed: %v", err)
	}

	// Should have exactly 1 level (C=1)
	if len(profile.Levels) != 1 {
		t.Errorf("expected 1 level for target=1, got %d", len(profile.Levels))
	}
}

func TestProfileSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")

	original := &warmup.Profile{
		Model:                "test-model",
		CreatedAt:            time.Now().Truncate(time.Second),
		WorkloadISL:          2048,
		WorkloadOSL:          256,
		TargetConcurrency:    64,
		KernelWarmupRequests: 3,
		TTFTMs:               45.2,
		TPOTModel:            warmup.QuadModel{A: 8.2, B: 0.15, C: 0.0008},
		Derived: warmup.DerivedMetrics{
			OptimalConcurrency: 101,
			MaxThroughputTokS:  4608,
			TwoxDegradationC:   186,
			Regime:             "bandwidth-bound",
		},
		Levels: []warmup.Level{
			{Concurrency: 1, Requests: 5, TTFTMs: warmup.LatencyStats{Mean: 44, P50: 44, P90: 46}, TPOTMs: warmup.LatencyStats{Mean: 8.1, P50: 8.1, P90: 8.5}},
		},
	}

	if err := warmup.SaveProfile(path, original); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	loaded, err := warmup.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}

	if loaded.Model != original.Model {
		t.Errorf("Model: got %q, want %q", loaded.Model, original.Model)
	}
	if loaded.KernelWarmupRequests != original.KernelWarmupRequests {
		t.Errorf("KernelWarmupRequests: got %d, want %d", loaded.KernelWarmupRequests, original.KernelWarmupRequests)
	}
	if math.Abs(loaded.TPOTModel.A-original.TPOTModel.A) > 1e-6 {
		t.Errorf("TPOTModel.A: got %f, want %f", loaded.TPOTModel.A, original.TPOTModel.A)
	}
	if loaded.Derived.Regime != original.Derived.Regime {
		t.Errorf("Derived.Regime: got %q, want %q", loaded.Derived.Regime, original.Derived.Regime)
	}

	// Verify JSON is well-formed
	data, _ := os.ReadFile(path)
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("saved JSON is malformed: %v", err)
	}
}
