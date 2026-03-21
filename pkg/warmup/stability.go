package warmup

import "math"

// IsStable returns true when the last `window` consecutive TTFT values
// have each changed by less than `threshold` fraction relative to their
// predecessor. For example, threshold=0.10 and window=2 requires the
// last 2 deltas to each be < 10%.
//
// Returns false if there are fewer than window+1 values.
func IsStable(ttfts []float64, threshold float64, window int) bool {
	if len(ttfts) < window+1 {
		return false
	}

	for i := len(ttfts) - window; i < len(ttfts); i++ {
		prev := ttfts[i-1]
		if prev == 0 {
			return false
		}
		delta := math.Abs(ttfts[i]-prev) / prev
		if delta >= threshold {
			return false
		}
	}
	return true
}
