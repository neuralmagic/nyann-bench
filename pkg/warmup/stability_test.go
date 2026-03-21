package warmup

import "testing"

func TestIsStableKernelCompilation(t *testing.T) {
	// Typical pattern: first request very slow (CUDA compile), then settles
	ttfts := []float64{500, 100, 92, 89}

	if IsStable(ttfts[:2], 0.10, 2) {
		t.Error("should not be stable with only 2 values (need 3 for window=2)")
	}
	if IsStable(ttfts[:3], 0.10, 2) {
		t.Error("500→100 is 80% change, should not be stable")
	}
	if !IsStable(ttfts, 0.10, 2) {
		t.Error("100→92 is 8% and 92→89 is 3.3%, should be stable")
	}
}

func TestIsStableIdentical(t *testing.T) {
	ttfts := []float64{50, 50, 50}
	if !IsStable(ttfts, 0.10, 2) {
		t.Error("identical values should be stable")
	}
}

func TestIsStableSingleValue(t *testing.T) {
	if IsStable([]float64{42}, 0.10, 2) {
		t.Error("single value should not be stable")
	}
}

func TestIsStableOscillating(t *testing.T) {
	ttfts := []float64{50, 80, 50, 80}
	if IsStable(ttfts, 0.10, 2) {
		t.Error("oscillating values should not be stable")
	}
}

func TestIsStableGradualConvergence(t *testing.T) {
	// Converges gradually: each step < 10% change
	ttfts := []float64{100, 95, 92, 90, 89}
	if !IsStable(ttfts, 0.10, 2) {
		t.Error("gradual convergence should be stable")
	}
}

func TestIsStableWindow1(t *testing.T) {
	ttfts := []float64{100, 95}
	if !IsStable(ttfts, 0.10, 1) {
		t.Error("5% change with window=1 and threshold=10% should be stable")
	}
}

func TestIsStableEmpty(t *testing.T) {
	if IsStable(nil, 0.10, 2) {
		t.Error("empty slice should not be stable")
	}
}
