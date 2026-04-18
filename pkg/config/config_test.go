package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func TestParseInline(t *testing.T) {
	sc, err := config.Parse(`{
		"load": {
			"mode": "concurrent",
			"concurrency": 100,
			"rampup": "30s",
			"duration": "5m"
		},
		"workload": {
			"type": "faker",
			"isl": 512,
			"osl": 1024,
			"turns": 3
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(sc.Stages))
	}
	s := sc.Stages[0]
	if s.Mode != "concurrent" {
		t.Errorf("expected concurrent, got %s", s.Mode)
	}
	if s.Concurrency != 100 {
		t.Errorf("expected 100, got %d", s.Concurrency)
	}
	if s.Rampup != 30*time.Second {
		t.Errorf("expected 30s rampup, got %v", s.Rampup)
	}
	if s.Duration != 5*time.Minute {
		t.Errorf("expected 5m duration, got %v", s.Duration)
	}
	if sc.Workload.ISL != 512 {
		t.Errorf("expected ISL 512, got %d", sc.Workload.ISL)
	}
	if sc.Workload.Turns != 3 {
		t.Errorf("expected 3 turns, got %d", sc.Workload.Turns)
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	err := os.WriteFile(path, []byte(`{
		"load": {"mode": "poisson", "rate": 50, "duration": "2m"},
		"workload": {"type": "synthetic", "isl": 64, "osl": 128}
	}`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	sc, err := config.Parse(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(sc.Stages))
	}
	if sc.Stages[0].Mode != "poisson" {
		t.Errorf("expected poisson, got %s", sc.Stages[0].Mode)
	}
	if sc.Stages[0].Rate != 50 {
		t.Errorf("expected rate 50, got %f", sc.Stages[0].Rate)
	}
}

func TestParseDefaults(t *testing.T) {
	sc, err := config.Parse(`{}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(sc.Stages))
	}
	if sc.Stages[0].Mode != "concurrent" {
		t.Errorf("expected default mode concurrent, got %s", sc.Stages[0].Mode)
	}
	if sc.Stages[0].Concurrency != 10 {
		t.Errorf("expected default concurrency 10, got %d", sc.Stages[0].Concurrency)
	}
	if sc.Stages[0].Duration != 60*time.Second {
		t.Errorf("expected default duration 60s, got %v", sc.Stages[0].Duration)
	}
	if sc.Workload.Type != "faker" {
		t.Errorf("expected default type faker, got %s", sc.Workload.Type)
	}
	if sc.Workload.ISL != 128 {
		t.Errorf("expected default ISL 128, got %d", sc.Workload.ISL)
	}
}

func TestDurationNumeric(t *testing.T) {
	sc, err := config.Parse(`{"load": {"duration": 120}}`)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Stages[0].Duration != 120*time.Second {
		t.Errorf("expected 120s, got %v", sc.Stages[0].Duration)
	}
}

func TestParseStarFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.star")
	err := os.WriteFile(path, []byte(`
scenario(
    stages = [stage("2m", concurrency=50)],
    workload = workload("synthetic", isl=256, osl=512),
)
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	sc, err := config.Parse(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(sc.Stages))
	}
	if sc.Stages[0].Concurrency != 50 {
		t.Errorf("expected concurrency 50, got %d", sc.Stages[0].Concurrency)
	}
	if sc.Workload.Type != "synthetic" {
		t.Errorf("expected synthetic, got %s", sc.Workload.Type)
	}
}

func TestParseJSONWithWarmup(t *testing.T) {
	sc, err := config.Parse(`{
		"warmup": {"duration": "30s", "stagger": true},
		"load": {"mode": "concurrent", "concurrency": 100, "duration": "5m"},
		"workload": {"type": "faker"}
	}`)
	if err != nil {
		t.Fatal(err)
	}

	// Should produce 2 stages: warmup + main
	if len(sc.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(sc.Stages))
	}

	// First stage is warmup
	if !sc.Stages[0].Warmup {
		t.Error("stage 0: expected warmup=true")
	}
	if sc.Stages[0].Duration != 30*time.Second {
		t.Errorf("stage 0: expected 30s, got %v", sc.Stages[0].Duration)
	}
	if sc.Stages[0].Rampup != 30*time.Second {
		t.Errorf("stage 0: expected rampup=30s (stagger), got %v", sc.Stages[0].Rampup)
	}
	if sc.Stages[0].Concurrency != 100 {
		t.Errorf("stage 0: expected concurrency=100 (from first main stage), got %d", sc.Stages[0].Concurrency)
	}

	// Second stage is main
	if sc.Stages[1].Warmup {
		t.Error("stage 1: should not be warmup")
	}
	if sc.Stages[1].Duration != 5*time.Minute {
		t.Errorf("stage 1: expected 5m, got %v", sc.Stages[1].Duration)
	}
}

func TestParseJSONWithSweep(t *testing.T) {
	sc, err := config.Parse(`{
		"sweep": {"min": 10, "max": 50, "steps": 5, "step_duration": "2m"},
		"workload": {"type": "faker"}
	}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 5 {
		t.Fatalf("expected 5 stages, got %d", len(sc.Stages))
	}

	expected := []int{10, 20, 30, 40, 50}
	for i, want := range expected {
		if sc.Stages[i].Concurrency != want {
			t.Errorf("stage %d: expected concurrency %d, got %d", i, want, sc.Stages[i].Concurrency)
		}
		if sc.Stages[i].Duration != 2*time.Minute {
			t.Errorf("stage %d: expected 2m, got %v", i, sc.Stages[i].Duration)
		}
	}
}

func TestBurstStages(t *testing.T) {
	cfg, err := config.Parse(`{
		"burst": {
			"concurrency": 64,
			"burst_duration": "5s",
			"pause_duration": "10s",
			"cycles": 3
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}

	stages := cfg.EffectiveStages()
	if len(stages) != 6 {
		t.Fatalf("expected 6 stages (3 cycles × 2), got %d", len(stages))
	}

	for i, stage := range stages {
		if i%2 == 0 {
			// Burst stage
			if stage.Concurrency != 64 {
				t.Errorf("stage %d: expected concurrency 64, got %d", i, stage.Concurrency)
			}
			if stage.Duration.Duration() != 5*time.Second {
				t.Errorf("stage %d: expected 5s, got %v", i, stage.Duration.Duration())
			}
		} else {
			// Pause stage
			if stage.Concurrency != 0 {
				t.Errorf("stage %d: expected concurrency 0, got %d", i, stage.Concurrency)
			}
			if stage.Duration.Duration() != 10*time.Second {
				t.Errorf("stage %d: expected 10s, got %v", i, stage.Duration.Duration())
			}
		}
	}
}

func TestBurstDefaultCycles(t *testing.T) {
	stages := config.BurstStages(&config.Burst{
		Concurrency:   32,
		BurstDuration: config.Duration(3 * time.Second),
		PauseDuration: config.Duration(7 * time.Second),
		Cycles:        0, // should default to 1
	})
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages (1 default cycle), got %d", len(stages))
	}
	if stages[0].Concurrency != 32 {
		t.Errorf("expected concurrency 32, got %d", stages[0].Concurrency)
	}
}

func TestBurstOverridesOtherModes(t *testing.T) {
	cfg, err := config.Parse(`{
		"burst": {"concurrency": 16, "burst_duration": "2s", "pause_duration": "3s", "cycles": 1},
		"sweep": {"min": 1, "max": 100, "steps": 10, "step_duration": "30s"},
		"stages": [{"concurrency": 50, "duration": "60s"}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	stages := cfg.EffectiveStages()
	if len(stages) != 2 {
		t.Fatalf("burst should take priority, got %d stages", len(stages))
	}
	if stages[0].Concurrency != 16 {
		t.Errorf("expected burst concurrency 16, got %d", stages[0].Concurrency)
	}
}
