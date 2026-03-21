package warmup

import (
	"testing"
	"time"
)

func TestPlanWarmupStagesBasic(t *testing.T) {
	profile := &Profile{
		KernelWarmupRequests: 3,
		WorkloadOSL:          256,
		TTFTMs:               50, // 50ms TTFT
		TPOTModel:            QuadModel{A: 10, B: 0.1, C: 0.001},
	}

	cfg := &PlanConfig{
		TargetConcurrency: 64,
		WorkloadOSL:       256,
	}

	stages := PlanWarmupStages(profile, cfg)

	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}

	// Stage 1: kernel warmup at C=1, no rampup
	if stages[0].Concurrency != 1 {
		t.Errorf("stage 0 concurrency: got %d, want 1", stages[0].Concurrency)
	}
	if stages[0].Duration < 2*time.Second {
		t.Errorf("stage 0 duration should be >= 2s, got %v", stages[0].Duration)
	}
	if stages[0].Rampup != 0 {
		t.Errorf("stage 0 rampup should be 0 (single stream), got %v", stages[0].Rampup)
	}

	// Stage 2: settle at target with staggered rampup
	if stages[1].Concurrency != 64 {
		t.Errorf("stage 1 concurrency: got %d, want 64", stages[1].Concurrency)
	}
	if stages[1].Duration < 5*time.Second {
		t.Errorf("stage 1 duration should be >= 5s (min settle), got %v", stages[1].Duration)
	}
	if stages[1].Duration > 120*time.Second {
		t.Errorf("stage 1 duration should be <= 120s (max settle), got %v", stages[1].Duration)
	}
	// Rampup should be ~one request lifetime
	if stages[1].Rampup <= 0 {
		t.Error("stage 1 rampup should be > 0 (stagger across request lifetime)")
	}
	if stages[1].Rampup >= stages[1].Duration {
		t.Errorf("rampup (%v) should be less than total duration (%v)",
			stages[1].Rampup, stages[1].Duration)
	}
}

func TestPlanWarmupStagesMinSettle(t *testing.T) {
	// Very fast engine: settle duration would be < 5s without clamping
	profile := &Profile{
		KernelWarmupRequests: 2,
		WorkloadOSL:          10,
		TTFTMs:               5,
		TPOTModel:            QuadModel{A: 1, B: 0.01, C: 0},
	}

	cfg := &PlanConfig{
		TargetConcurrency: 4,
		WorkloadOSL:       10,
	}

	stages := PlanWarmupStages(profile, cfg)

	if stages[1].Duration < 5*time.Second {
		t.Errorf("settle should be clamped to min 5s, got %v", stages[1].Duration)
	}
}

func TestPlanWarmupStagesMaxSettle(t *testing.T) {
	// Very slow engine: settle duration would be > 120s without clamping
	profile := &Profile{
		KernelWarmupRequests: 3,
		WorkloadOSL:          4096,
		TTFTMs:               5000,
		TPOTModel:            QuadModel{A: 50, B: 1, C: 0.01},
	}

	cfg := &PlanConfig{
		TargetConcurrency: 64,
		WorkloadOSL:       4096,
	}

	stages := PlanWarmupStages(profile, cfg)

	if stages[1].Duration > 120*time.Second {
		t.Errorf("settle should be clamped to max 120s, got %v", stages[1].Duration)
	}
}

func TestPlanWarmupStagesConcurrency1(t *testing.T) {
	profile := &Profile{
		KernelWarmupRequests: 3,
		WorkloadOSL:          100,
		TTFTMs:               20,
		TPOTModel:            QuadModel{A: 5, B: 0, C: 0},
	}

	cfg := &PlanConfig{
		TargetConcurrency: 1,
		WorkloadOSL:       100,
	}

	stages := PlanWarmupStages(profile, cfg)

	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}
	if stages[0].Concurrency != 1 {
		t.Errorf("stage 0: got C=%d, want 1", stages[0].Concurrency)
	}
	if stages[1].Concurrency != 1 {
		t.Errorf("stage 1: got C=%d, want 1", stages[1].Concurrency)
	}
}

func TestPlanWarmupStagesUsesModel(t *testing.T) {
	// Verify the settle duration and rampup increase with target concurrency
	// (because TPOT increases with C → longer request lifetime)
	profile := &Profile{
		KernelWarmupRequests: 3,
		WorkloadOSL:          256,
		TTFTMs:               50,
		TPOTModel:            QuadModel{A: 10, B: 0.5, C: 0.01},
	}

	stagesLow := PlanWarmupStages(profile, &PlanConfig{
		TargetConcurrency: 8,
		WorkloadOSL:       256,
	})
	stagesHigh := PlanWarmupStages(profile, &PlanConfig{
		TargetConcurrency: 64,
		WorkloadOSL:       256,
	})

	// Higher concurrency → higher TPOT → longer request lifetime → longer rampup + settle
	if stagesHigh[1].Duration <= stagesLow[1].Duration {
		t.Errorf("higher concurrency should produce longer settle: C=8 %v, C=64 %v",
			stagesLow[1].Duration, stagesHigh[1].Duration)
	}
	if stagesHigh[1].Rampup <= stagesLow[1].Rampup {
		t.Errorf("higher concurrency should produce longer rampup: C=8 %v, C=64 %v",
			stagesLow[1].Rampup, stagesHigh[1].Rampup)
	}
}

func TestPlanWarmupStagesRampupIsOneLifetime(t *testing.T) {
	// Verify rampup ≈ one request lifetime at target concurrency
	profile := &Profile{
		KernelWarmupRequests: 3,
		WorkloadOSL:          100,
		TTFTMs:               50, // 50ms
		TPOTModel:            QuadModel{A: 10, B: 0, C: 0}, // constant 10ms TPOT
	}

	stages := PlanWarmupStages(profile, &PlanConfig{
		TargetConcurrency: 16,
		WorkloadOSL:       100,
	})

	// Request lifetime = TTFT + TPOT × OSL = 50 + 10×100 = 1050ms
	expectedLifetime := 1050 * time.Millisecond
	tolerance := 50 * time.Millisecond

	rampup := stages[1].Rampup
	diff := rampup - expectedLifetime
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Errorf("rampup should be ~%v (one request lifetime), got %v", expectedLifetime, rampup)
	}
}
