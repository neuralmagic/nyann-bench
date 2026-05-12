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

func TestDivideConcurrency(t *testing.T) {
	tests := []struct {
		total, workers, id, want int
	}{
		{10, 3, 0, 4},
		{10, 3, 1, 3},
		{10, 3, 2, 3},
		{10, 1, 0, 10},
		{1, 3, 0, 1},
		{1, 3, 1, 0},
		{1, 3, 2, 0},
		{9, 3, 0, 3},
		{9, 3, 1, 3},
		{9, 3, 2, 3},
		{64, 4, 0, 16},
		{64, 4, 3, 16},
	}
	for _, tt := range tests {
		got := config.DivideConcurrency(tt.total, tt.workers, tt.id)
		if got != tt.want {
			t.Errorf("DivideConcurrency(%d, %d, %d) = %d, want %d",
				tt.total, tt.workers, tt.id, got, tt.want)
		}
	}
}

func TestDivideRate(t *testing.T) {
	got := config.DivideRate(100.0, 3)
	if got < 33.33 || got > 33.34 {
		t.Errorf("DivideRate(100, 3) = %f, want ~33.33", got)
	}
	if config.DivideRate(100.0, 1) != 100.0 {
		t.Errorf("DivideRate(100, 1) should return 100")
	}
	if config.DivideRate(100.0, 4) != 25.0 {
		t.Errorf("DivideRate(100, 4) should return 25")
	}
}

func TestInsertImplicitBarrier(t *testing.T) {
	sc := &config.ScenarioConfig{
		Stages: []config.ScenarioStage{
			{Warmup: true, Duration: 2 * time.Minute, Concurrency: 16},
			{Duration: 5 * time.Minute, Concurrency: 64},
			{Duration: 5 * time.Minute, Concurrency: 128},
		},
	}

	sc.InsertImplicitBarrier()

	if len(sc.Stages) != 4 {
		t.Fatalf("expected 4 stages after implicit barrier, got %d", len(sc.Stages))
	}
	if !sc.Stages[0].Warmup {
		t.Error("stage 0: expected warmup")
	}
	if !sc.Stages[1].Barrier {
		t.Error("stage 1: expected implicit barrier")
	}
	if !sc.Stages[1].BarrierDrain {
		t.Error("stage 1: implicit barrier should have drain=true")
	}
	if sc.Stages[2].Concurrency != 64 {
		t.Errorf("stage 2: expected concurrency 64, got %d", sc.Stages[2].Concurrency)
	}
}

func TestInsertImplicitBarrierSkipsIfPresent(t *testing.T) {
	sc := &config.ScenarioConfig{
		Stages: []config.ScenarioStage{
			{Warmup: true, Duration: 2 * time.Minute},
			{Barrier: true},
			{Duration: 5 * time.Minute, Concurrency: 64},
		},
	}

	sc.InsertImplicitBarrier()

	// Should not insert a second barrier
	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages (no duplicate barrier), got %d", len(sc.Stages))
	}
}

func TestInsertImplicitBarrierNoWarmup(t *testing.T) {
	sc := &config.ScenarioConfig{
		Stages: []config.ScenarioStage{
			{Duration: 5 * time.Minute, Concurrency: 64},
			{Duration: 5 * time.Minute, Concurrency: 128},
		},
	}

	sc.InsertImplicitBarrier()

	// Should insert barrier at position 0
	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(sc.Stages))
	}
	if !sc.Stages[0].Barrier {
		t.Error("stage 0: expected implicit barrier")
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
