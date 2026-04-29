package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func writeStarFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.star")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStarlarkMinimal(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s", concurrency=10)],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(sc.Stages))
	}
	if sc.Stages[0].Duration != 60*time.Second {
		t.Errorf("expected 60s, got %v", sc.Stages[0].Duration)
	}
	if sc.Stages[0].Concurrency != 10 {
		t.Errorf("expected concurrency 10, got %d", sc.Stages[0].Concurrency)
	}
	// Default workload
	if sc.Workload.Type != "faker" {
		t.Errorf("expected default workload faker, got %s", sc.Workload.Type)
	}
}

func TestStarlarkWorkloadTypes(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s")],
    workload = workload("synthetic", isl=512, osl=1024, turns=3),
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Workload.Type != "synthetic" {
		t.Errorf("expected synthetic, got %s", sc.Workload.Type)
	}
	if sc.Workload.ISL != 512 {
		t.Errorf("expected ISL 512, got %d", sc.Workload.ISL)
	}
	if sc.Workload.OSL != 1024 {
		t.Errorf("expected OSL 1024, got %d", sc.Workload.OSL)
	}
	if sc.Workload.Turns != 3 {
		t.Errorf("expected 3 turns, got %d", sc.Workload.Turns)
	}
}

func TestStarlarkSubsequentISL(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s")],
    workload = workload("faker", isl=256, osl=512, subsequent_isl=64),
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Workload.SubsequentISL == nil {
		t.Fatal("expected subsequent_isl to be set")
	}
	if *sc.Workload.SubsequentISL != 64 {
		t.Errorf("expected subsequent_isl 64, got %d", *sc.Workload.SubsequentISL)
	}
}

func TestStarlarkCacheSalt(t *testing.T) {
	tests := []struct {
		name      string
		saltExpr  string
		wantMode  string
		wantValue string
	}{
		{"random", `"random"`, "random", ""},
		{"fixed", `"fixed:abc123"`, "fixed", "abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeStarFile(t, `
scenario(
    stages = [stage("60s")],
    workload = workload("faker", cache_salt=`+tt.saltExpr+`),
)
`)
			sc, err := config.ParseStarlark(path)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Workload.CacheSalt == nil {
				t.Fatal("expected cache_salt to be set")
			}
			if sc.Workload.CacheSalt.Mode != tt.wantMode {
				t.Errorf("expected mode %q, got %q", tt.wantMode, sc.Workload.CacheSalt.Mode)
			}
			if sc.Workload.CacheSalt.Value != tt.wantValue {
				t.Errorf("expected value %q, got %q", tt.wantValue, sc.Workload.CacheSalt.Value)
			}
		})
	}
}

func TestStarlarkPerStageOverrides(t *testing.T) {
	path := writeStarFile(t, `
chat = workload("faker", isl=256, osl=512, name="chat")
long = workload("corpus", corpus_path="/data/text.txt", isl=2048, osl=512, name="long")

scenario(
    stages = [
        stage("30s", concurrency=16, target="http://main:8000/v1", warmup=True, name="warmup"),
        stage("5m", concurrency=128, target="http://main:8000/v1", workload=chat, name="chat"),
        stage("5m", concurrency=64, target="http://canary:8000/v1", workload=long, name="long"),
    ],
    workload = chat,
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(sc.Stages))
	}

	// Stage 0: warmup
	s0 := sc.Stages[0]
	if !s0.Warmup {
		t.Error("stage 0: expected warmup=true")
	}
	if s0.Name != "warmup" {
		t.Errorf("stage 0: expected name 'warmup', got %q", s0.Name)
	}
	if s0.Target != "http://main:8000/v1" {
		t.Errorf("stage 0: wrong target %q", s0.Target)
	}
	if s0.Workload != nil {
		t.Error("stage 0: expected nil workload (inherit from scenario)")
	}

	// Stage 1: chat with explicit workload
	s1 := sc.Stages[1]
	if s1.Warmup {
		t.Error("stage 1: should not be warmup")
	}
	if s1.Workload == nil {
		t.Fatal("stage 1: expected workload override")
	}
	if s1.Workload.Name != "chat" {
		t.Errorf("stage 1: expected workload name 'chat', got %q", s1.Workload.Name)
	}

	// Stage 2: different target and workload
	s2 := sc.Stages[2]
	if s2.Target != "http://canary:8000/v1" {
		t.Errorf("stage 2: wrong target %q", s2.Target)
	}
	if s2.Workload == nil {
		t.Fatal("stage 2: expected workload override")
	}
	if s2.Workload.Type != "corpus" {
		t.Errorf("stage 2: expected corpus workload, got %q", s2.Workload.Type)
	}
}

func TestStarlarkDurationFormats(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [
        stage("2m30s", concurrency=10),
        stage(120, concurrency=20),
    ],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Stages[0].Duration != 2*time.Minute+30*time.Second {
		t.Errorf("expected 2m30s, got %v", sc.Stages[0].Duration)
	}
	if sc.Stages[1].Duration != 120*time.Second {
		t.Errorf("expected 120s, got %v", sc.Stages[1].Duration)
	}
}

func TestStarlarkForLoop(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("2m", concurrency=c) for c in range(10, 51, 10)],
    workload = workload("faker", isl=512, osl=1024),
)
`)
	sc, err := config.ParseStarlark(path)
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
	}
}

func TestStarlarkFunctions(t *testing.T) {
	path := writeStarFile(t, `
def ramp(start, end, step, duration):
    return [stage(duration, concurrency=c) for c in range(start, end + 1, step)]

scenario(
    stages = ramp(10, 30, 10, "2m"),
    workload = workload("faker"),
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(sc.Stages))
	}
	expected := []int{10, 20, 30}
	for i, want := range expected {
		if sc.Stages[i].Concurrency != want {
			t.Errorf("stage %d: expected %d, got %d", i, want, sc.Stages[i].Concurrency)
		}
	}
}

func TestStarlarkScenarioTarget(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s")],
    target = "http://myhost:8000/v1",
    model = "my-model",
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Target != "http://myhost:8000/v1" {
		t.Errorf("expected target, got %q", sc.Target)
	}
	if sc.Model != "my-model" {
		t.Errorf("expected model, got %q", sc.Model)
	}
}

func TestStarlarkRampup(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s", concurrency=100, rampup="30s")],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Stages[0].Rampup != 30*time.Second {
		t.Errorf("expected 30s rampup, got %v", sc.Stages[0].Rampup)
	}
}

func TestStarlarkModes(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [
        stage("60s", concurrency=10, mode="concurrent"),
        stage("60s", rate=50.0, mode="constant"),
        stage("60s", rate=50.0, mode="poisson", max_inflight=100),
    ],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Stages[0].Mode != "concurrent" {
		t.Errorf("stage 0: expected concurrent, got %s", sc.Stages[0].Mode)
	}
	if sc.Stages[1].Mode != "constant" {
		t.Errorf("stage 1: expected constant, got %s", sc.Stages[1].Mode)
	}
	if sc.Stages[1].Rate != 50.0 {
		t.Errorf("stage 1: expected rate 50, got %f", sc.Stages[1].Rate)
	}
	if sc.Stages[2].Mode != "poisson" {
		t.Errorf("stage 2: expected poisson, got %s", sc.Stages[2].Mode)
	}
	if sc.Stages[2].MaxInFlight != 100 {
		t.Errorf("stage 2: expected max_inflight 100, got %d", sc.Stages[2].MaxInFlight)
	}
}

// Error cases

func TestStarlarkNoScenario(t *testing.T) {
	path := writeStarFile(t, `
w = workload("faker")
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for missing scenario()")
	}
	if got := err.Error(); !contains(got, "no scenario() call found") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkDoubleScenario(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[stage("60s")])
scenario(stages=[stage("60s")])
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for double scenario()")
	}
	if got := err.Error(); !contains(got, "scenario() can only be called once") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkEmptyStages(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[])
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for empty stages")
	}
	if got := err.Error(); !contains(got, "stages must contain at least one stage") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkInvalidWorkloadType(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[stage("60s")], workload=workload("bad"))
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for invalid workload type")
	}
	if got := err.Error(); !contains(got, "unknown workload type") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkCorpusMissingPath(t *testing.T) {
	path := writeStarFile(t, `
workload("corpus")
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for missing corpus_path")
	}
	if got := err.Error(); !contains(got, "corpus_path is required") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkGSM8KMissingPath(t *testing.T) {
	path := writeStarFile(t, `
workload("gsm8k")
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for missing gsm8k_path")
	}
	if got := err.Error(); !contains(got, "gsm8k_path is required") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkGSM8KMissingTrainPath(t *testing.T) {
	path := writeStarFile(t, `
workload("gsm8k", gsm8k_path="/data/test.jsonl", num_fewshot=5)
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for missing gsm8k_train_path")
	}
	if got := err.Error(); !contains(got, "gsm8k_train_path is required") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkWrongTypeInStages(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[workload("faker")])
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for wrong type in stages")
	}
	if got := err.Error(); !contains(got, "expected Stage or barrier()") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkInvalidDuration(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[stage("not-a-duration")])
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestStarlarkInvalidMode(t *testing.T) {
	path := writeStarFile(t, `
scenario(stages=[stage("60s", mode="bad")])
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if got := err.Error(); !contains(got, "unknown mode") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestStarlarkGSM8KZeroShot(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s")],
    workload = workload("gsm8k", gsm8k_path="/data/test.jsonl", num_fewshot=0),
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Workload.NumFewShot == nil {
		t.Fatal("expected num_fewshot to be set")
	}
	if *sc.Workload.NumFewShot != 0 {
		t.Errorf("expected 0, got %d", *sc.Workload.NumFewShot)
	}
}

func TestStarlarkMaxRequests(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("30m", concurrency=64, max_requests=1319)],
    workload = workload("faker"),
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Stages[0].MaxRequests != 1319 {
		t.Errorf("expected max_requests 1319, got %d", sc.Stages[0].MaxRequests)
	}
}

func TestStarlarkMaxRequestsDefault(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [stage("60s", concurrency=10)],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if sc.Stages[0].MaxRequests != 0 {
		t.Errorf("expected default max_requests 0, got %d", sc.Stages[0].MaxRequests)
	}
}

func TestStarlarkWorkloadImmutable(t *testing.T) {
	path := writeStarFile(t, `
w = workload("faker")
w.isl = 999
`)
	_, err := config.ParseStarlark(path)
	if err == nil {
		t.Fatal("expected error for mutating workload")
	}
}

// End-to-end: complex realistic scenario
func TestStarlarkComplexScenario(t *testing.T) {
	path := writeStarFile(t, `
light = workload("faker", isl=128, osl=256, name="light")
heavy = workload("faker", isl=1024, osl=2048, turns=3, name="heavy")

scenario(
    stages = (
        [stage("30s", concurrency=c, warmup=True) for c in range(10, 51, 10)] +
        [stage("10m", concurrency=100)] +
        [stage("10m", concurrency=100, workload=heavy)] +
        [stage("2m", concurrency=500, workload=heavy)] +
        [stage("30s", concurrency=c) for c in range(100, 9, -10)]
    ),
    workload = light,
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	// 5 warmup + 1 steady + 1 heavy + 1 spike + 10 cooldown = 18
	if len(sc.Stages) != 18 {
		t.Fatalf("expected 18 stages, got %d", len(sc.Stages))
	}

	// First 5 are warmup
	for i := 0; i < 5; i++ {
		if !sc.Stages[i].Warmup {
			t.Errorf("stage %d: expected warmup", i)
		}
	}

	// Stage 5 is steady state
	if sc.Stages[5].Concurrency != 100 {
		t.Errorf("stage 5: expected concurrency 100, got %d", sc.Stages[5].Concurrency)
	}
	if sc.Stages[5].Warmup {
		t.Error("stage 5: should not be warmup")
	}

	// Stage 6 has heavy workload override
	if sc.Stages[6].Workload == nil {
		t.Fatal("stage 6: expected workload override")
	}
	if sc.Stages[6].Workload.Name != "heavy" {
		t.Errorf("stage 6: expected heavy workload, got %q", sc.Stages[6].Workload.Name)
	}

	// Stage 7 is the spike
	if sc.Stages[7].Concurrency != 500 {
		t.Errorf("stage 7: expected concurrency 500, got %d", sc.Stages[7].Concurrency)
	}

	// Default workload is light
	if sc.Workload.Name != "light" {
		t.Errorf("expected default workload name 'light', got %q", sc.Workload.Name)
	}
}

func TestStarlarkBarrier(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [
        stage("2m", concurrency=16, warmup=True),
        barrier(),
        stage("5m", concurrency=64),
    ],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(sc.Stages))
	}

	if !sc.Stages[0].Warmup {
		t.Error("stage 0: expected warmup")
	}
	if !sc.Stages[1].Barrier {
		t.Error("stage 1: expected barrier")
	}
	if sc.Stages[1].BarrierDrain {
		t.Error("stage 1: barrier should not drain by default")
	}
	if sc.Stages[2].Barrier {
		t.Error("stage 2: should not be barrier")
	}
	if sc.Stages[2].Concurrency != 64 {
		t.Errorf("stage 2: expected concurrency 64, got %d", sc.Stages[2].Concurrency)
	}
}

func TestStarlarkBarrierDrain(t *testing.T) {
	path := writeStarFile(t, `
scenario(
    stages = [
        stage("5m", concurrency=64),
        barrier(drain=True),
        stage("5m", concurrency=64),
    ],
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(sc.Stages))
	}

	if !sc.Stages[1].Barrier {
		t.Error("stage 1: expected barrier")
	}
	if !sc.Stages[1].BarrierDrain {
		t.Error("stage 1: expected barrier drain=true")
	}
}

func TestStarlarkBarrierInComplex(t *testing.T) {
	path := writeStarFile(t, `
chat = workload("faker", isl=256, osl=512)
coding = workload("faker", isl=1024, osl=2048)

scenario(
    stages = [
        stage("2m", concurrency=16, warmup=True),
        barrier(),
        stage("5m", concurrency=64),
        stage("5m", concurrency=128),
        barrier(drain=True),
        stage("5m", concurrency=64, workload=coding),
    ],
    workload = chat,
)
`)
	sc, err := config.ParseStarlark(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.Stages) != 6 {
		t.Fatalf("expected 6 stages, got %d", len(sc.Stages))
	}

	// barrier at index 1 (no drain)
	if !sc.Stages[1].Barrier || sc.Stages[1].BarrierDrain {
		t.Error("stage 1: expected barrier without drain")
	}
	// barrier at index 4 (drain)
	if !sc.Stages[4].Barrier || !sc.Stages[4].BarrierDrain {
		t.Error("stage 4: expected barrier with drain")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsImpl(s, substr)
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
