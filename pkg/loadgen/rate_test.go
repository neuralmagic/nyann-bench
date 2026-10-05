package loadgen

import (
	"testing"
	"time"
)

func TestArrivalGapLinearRamp(t *testing.T) {
	g := &Generator{Rate: 100}
	for _, tc := range []struct {
		name    string
		elapsed float64
		rampup  time.Duration
		arrival float64
		want    time.Duration
	}{
		{"no ramp", 0, 0, 1, 10 * time.Millisecond},
		{"ramp start", 0, time.Second, 0.5, 100 * time.Millisecond},
		{"within ramp", 0.3, time.Second, 3.5, 100 * time.Millisecond},
		{"across ramp end", 0.9, time.Second, 19.5, 200 * time.Millisecond},
		{"after ramp", 2, time.Second, 2, 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := g.arrivalGap(tc.elapsed, tc.rampup, tc.arrival)
			if delta := got - tc.want; delta < -time.Nanosecond || delta > time.Nanosecond {
				t.Errorf("arrival gap = %v, want %v", got, tc.want)
			}
		})
	}
}
