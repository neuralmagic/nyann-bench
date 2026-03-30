package warmup_test

import (
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/warmup"
)

func TestStage(t *testing.T) {
	cfg := &warmup.Config{
		Duration:    30 * time.Second,
		Concurrency: 8,
		Stagger:     false,
	}

	stage, err := warmup.Stage(cfg)
	if err != nil {
		t.Fatalf("Stage failed: %v", err)
	}

	if stage.Concurrency != 8 {
		t.Errorf("concurrency: got %d, want 8", stage.Concurrency)
	}
	if stage.Duration != 30*time.Second {
		t.Errorf("duration: got %v, want 30s", stage.Duration)
	}
	if stage.Rampup != 0 {
		t.Errorf("rampup should be 0 without stagger, got %v", stage.Rampup)
	}
}

func TestStageWithStagger(t *testing.T) {
	cfg := &warmup.Config{
		Duration:    30 * time.Second,
		Concurrency: 8,
		Stagger:     true,
	}

	stage, err := warmup.Stage(cfg)
	if err != nil {
		t.Fatalf("Stage failed: %v", err)
	}

	if stage.Rampup != 30*time.Second {
		t.Errorf("rampup should equal duration with stagger, got %v", stage.Rampup)
	}
}

func TestStageZeroDuration(t *testing.T) {
	cfg := &warmup.Config{
		Duration:    0,
		Concurrency: 8,
	}

	_, err := warmup.Stage(cfg)
	if err == nil {
		t.Fatal("expected error for zero duration")
	}
}

func TestStageZeroConcurrency(t *testing.T) {
	cfg := &warmup.Config{
		Duration:    10 * time.Second,
		Concurrency: 0,
	}

	_, err := warmup.Stage(cfg)
	if err == nil {
		t.Fatal("expected error for zero concurrency")
	}
}
