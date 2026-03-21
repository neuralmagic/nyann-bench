package warmup

import (
	"math"
	"testing"
)

func TestFitQuadraticPerfect(t *testing.T) {
	// y = 10 + 2x + 0.5x²
	xs := []float64{0, 5, 10}
	ys := make([]float64, len(xs))
	for i, x := range xs {
		ys[i] = 10 + 2*x + 0.5*x*x
	}

	m := FitQuadratic(xs, ys)

	if math.Abs(m.A-10) > 1e-6 {
		t.Errorf("A: got %f, want 10", m.A)
	}
	if math.Abs(m.B-2) > 1e-6 {
		t.Errorf("B: got %f, want 2", m.B)
	}
	if math.Abs(m.C-0.5) > 1e-6 {
		t.Errorf("C: got %f, want 0.5", m.C)
	}
}

func TestFitQuadraticOverdetermined(t *testing.T) {
	// 5 points from y = 5 + 0.1x + 0.01x², should fit closely
	xs := []float64{1, 4, 8, 16, 32}
	ys := make([]float64, len(xs))
	for i, x := range xs {
		ys[i] = 5 + 0.1*x + 0.01*x*x
	}

	m := FitQuadratic(xs, ys)

	if math.Abs(m.A-5) > 0.01 {
		t.Errorf("A: got %f, want ~5", m.A)
	}
	if math.Abs(m.B-0.1) > 0.01 {
		t.Errorf("B: got %f, want ~0.1", m.B)
	}
	if math.Abs(m.C-0.01) > 0.001 {
		t.Errorf("C: got %f, want ~0.01", m.C)
	}
}

func TestFitQuadraticLinearData(t *testing.T) {
	// y = 3 + 2x (no curvature), C should be ~0
	xs := []float64{1, 10, 20, 30}
	ys := make([]float64, len(xs))
	for i, x := range xs {
		ys[i] = 3 + 2*x
	}

	m := FitQuadratic(xs, ys)

	if math.Abs(m.C) > 1e-6 {
		t.Errorf("C should be ~0 for linear data, got %f", m.C)
	}
	if math.Abs(m.A-3) > 0.01 {
		t.Errorf("A: got %f, want ~3", m.A)
	}
	if math.Abs(m.B-2) > 0.01 {
		t.Errorf("B: got %f, want ~2", m.B)
	}
}

func TestFitLinear(t *testing.T) {
	xs := []float64{1, 64}
	ys := []float64{8.0, 20.0}

	m := FitLinear(xs, ys)

	if m.C != 0 {
		t.Errorf("C should be 0 for linear fit, got %f", m.C)
	}

	// y(1) should be 8, y(64) should be 20
	if math.Abs(m.Predict(1)-8) > 1e-6 {
		t.Errorf("Predict(1) = %f, want 8", m.Predict(1))
	}
	if math.Abs(m.Predict(64)-20) > 1e-6 {
		t.Errorf("Predict(64) = %f, want 20", m.Predict(64))
	}
}

func TestFitLinearSinglePoint(t *testing.T) {
	m := FitLinear([]float64{5}, []float64{42})

	if m.A != 42 {
		t.Errorf("A: got %f, want 42", m.A)
	}
	if m.B != 0 {
		t.Errorf("B should be 0 for single point, got %f", m.B)
	}
}

func TestFitLinearEmpty(t *testing.T) {
	m := FitLinear(nil, nil)
	if m.A != 0 || m.B != 0 || m.C != 0 {
		t.Errorf("empty fit should be zero model, got %+v", m)
	}
}

func TestComputeDerivedBandwidthBound(t *testing.T) {
	// a=10, b=0.1, c=0.001 → C_opt = sqrt(10/0.001) = 100
	model := QuadModel{A: 10, B: 0.1, C: 0.001}
	d := ComputeDerived(model, 32)

	if d.OptimalConcurrency != 100 {
		t.Errorf("OptimalConcurrency: got %d, want 100", d.OptimalConcurrency)
	}
	if d.Regime != "bandwidth-bound" {
		t.Errorf("Regime at C=32: got %q, want bandwidth-bound", d.Regime)
	}
	if d.TwoxDegradationC <= 0 {
		t.Error("TwoxDegradationC should be positive")
	}
	if d.MaxThroughputTokS <= 0 {
		t.Error("MaxThroughputTokS should be positive")
	}
}

func TestComputeDerivedComputeBound(t *testing.T) {
	model := QuadModel{A: 10, B: 0.1, C: 0.01}
	// At C=64: c*C² = 0.01*4096 = 40.96 > a=10 → compute-bound
	d := ComputeDerived(model, 64)

	if d.Regime != "compute-bound" {
		t.Errorf("Regime at C=64: got %q, want compute-bound", d.Regime)
	}
}

func TestComputeDerivedLinearModel(t *testing.T) {
	// c=0, linear model
	model := QuadModel{A: 10, B: 0.5, C: 0}
	d := ComputeDerived(model, 32)

	// With c=0, no optimal concurrency from sqrt(a/c)
	if d.OptimalConcurrency != 0 {
		t.Errorf("OptimalConcurrency should be 0 for linear model, got %d", d.OptimalConcurrency)
	}
	// 2x degradation: b*C = a → C = 10/0.5 = 20
	if d.TwoxDegradationC != 20 {
		t.Errorf("TwoxDegradationC: got %d, want 20", d.TwoxDegradationC)
	}
	if d.Regime != "bandwidth-bound" {
		t.Errorf("Regime: got %q, want bandwidth-bound", d.Regime)
	}
}

func TestQuadModelPredict(t *testing.T) {
	m := QuadModel{A: 5, B: 0.2, C: 0.01}

	// f(0) = 5
	if v := m.Predict(0); v != 5 {
		t.Errorf("Predict(0) = %f, want 5", v)
	}
	// f(10) = 5 + 2 + 1 = 8
	if v := m.Predict(10); math.Abs(v-8) > 1e-10 {
		t.Errorf("Predict(10) = %f, want 8", v)
	}
}
