package warmup_test

import (
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/config"
	"github.com/neuralmagic/nyann_poker/pkg/warmup"
)

func TestSingleStage(t *testing.T) {
	cfgStages := []config.WarmupStage{
		{Duration: config.Duration(30 * time.Second)},
	}

	stages, err := warmup.Stages(cfgStages, 8)
	if err != nil {
		t.Fatalf("Stages failed: %v", err)
	}

	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0].Concurrency != 8 {
		t.Errorf("concurrency: got %d, want 8 (inherited)", stages[0].Concurrency)
	}
	if stages[0].Duration != 30*time.Second {
		t.Errorf("duration: got %v, want 30s", stages[0].Duration)
	}
	if stages[0].Rampup != 0 {
		t.Errorf("rampup should be 0 without stagger, got %v", stages[0].Rampup)
	}
}

func TestMultiStagePreflightThenSettle(t *testing.T) {
	cfgStages := []config.WarmupStage{
		{Duration: config.Duration(60 * time.Second), Concurrency: 1},
		{Duration: config.Duration(30 * time.Second), Stagger: true},
	}

	stages, err := warmup.Stages(cfgStages, 8)
	if err != nil {
		t.Fatalf("Stages failed: %v", err)
	}

	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}

	// Preflight: concurrency=1, no stagger
	if stages[0].Concurrency != 1 {
		t.Errorf("stage 0 concurrency: got %d, want 1", stages[0].Concurrency)
	}
	if stages[0].Duration != 60*time.Second {
		t.Errorf("stage 0 duration: got %v, want 60s", stages[0].Duration)
	}
	if stages[0].Rampup != 0 {
		t.Errorf("stage 0 rampup should be 0, got %v", stages[0].Rampup)
	}

	// Settle: inherited concurrency=8, stagger
	if stages[1].Concurrency != 8 {
		t.Errorf("stage 1 concurrency: got %d, want 8 (inherited)", stages[1].Concurrency)
	}
	if stages[1].Rampup != 30*time.Second {
		t.Errorf("stage 1 rampup should equal duration with stagger, got %v", stages[1].Rampup)
	}
}

func TestZeroDurationError(t *testing.T) {
	cfgStages := []config.WarmupStage{
		{Duration: 0, Concurrency: 8},
	}

	_, err := warmup.Stages(cfgStages, 8)
	if err == nil {
		t.Fatal("expected error for zero duration")
	}
}

func TestEmptyStagesError(t *testing.T) {
	_, err := warmup.Stages(nil, 8)
	if err == nil {
		t.Fatal("expected error for empty stages")
	}
}
