package loadgen

import (
	"math"
	mathrand "math/rand"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func TestThinkTimeLognormal(t *testing.T) {
	const (
		n      = 40000
		median = time.Second
		sigma  = 1.0
		limit  = 5 * time.Second
	)
	g := &Generator{ThinkTime: &config.ThinkTime{
		Median: config.Duration(median),
		Sigma:  sigma,
		Max:    config.Duration(limit),
	}}
	r := mathrand.New(mathrand.NewSource(1))
	draws := make([]time.Duration, n)
	capped := 0
	for i := range draws {
		d := g.thinkTime(r)
		if d <= 0 || d > limit {
			t.Fatalf("draw %d = %s, want in (0, %s]", i, d, limit)
		}
		if d == limit {
			capped++
		}
		draws[i] = d
	}
	sort.Slice(draws, func(i, j int) bool { return draws[i] < draws[j] })

	if got := draws[n/2]; math.Abs(got.Seconds()-median.Seconds()) > 0.05 {
		t.Fatalf("empirical median %s, want %s +- 50ms", got, median)
	}
	want := 0.5 * math.Erfc(math.Log(limit.Seconds()/median.Seconds())/sigma/math.Sqrt2)
	if got := float64(capped) / n; math.Abs(got-want) > 0.006 {
		t.Fatalf("share of draws at the cap %.4f, want %.4f", got, want)
	}
}

func TestThinkTimeConstantWithZeroSigma(t *testing.T) {
	g := &Generator{ThinkTime: &config.ThinkTime{Median: config.Duration(250 * time.Millisecond)}}
	r := mathrand.New(mathrand.NewSource(1))
	for i := 0; i < 100; i++ {
		if d := g.thinkTime(r); d != 250*time.Millisecond {
			t.Fatalf("draw %d = %s, want 250ms", i, d)
		}
	}
}

func TestThinkTimeUncappedWithZeroMax(t *testing.T) {
	g := &Generator{ThinkTime: &config.ThinkTime{Median: config.Duration(time.Second), Sigma: 2}}
	r := mathrand.New(mathrand.NewSource(1))
	var longest time.Duration
	for i := 0; i < 10000; i++ {
		longest = max(longest, g.thinkTime(r))
	}
	if longest < 100*time.Second {
		t.Fatalf("longest draw %s; expected an uncapped tail", longest)
	}
}

func TestThinkTimeNilIsZero(t *testing.T) {
	if d := (&Generator{}).thinkTime(nil); d != 0 {
		t.Fatalf("thinkTime() = %s with no config, want 0", d)
	}
}

func TestSeededStreamsReplay(t *testing.T) {
	draws := func(g *Generator, key string) []float64 {
		r := g.rng(key)
		out := make([]float64, 20)
		for i := range out {
			out[i] = r.Float64()
		}
		return out
	}
	seeded := &Generator{Seed: 42}
	first := draws(seeded, "think/w3-c3")
	if again := draws(&Generator{Seed: 42}, "think/w3-c3"); !slices.Equal(first, again) {
		t.Fatal("same seed and key produced different draws")
	}
	if other := draws(seeded, "think/w4-c4"); slices.Equal(first, other) {
		t.Fatal("different keys produced the same draws")
	}
	if other := draws(&Generator{Seed: 43}, "think/w3-c3"); slices.Equal(first, other) {
		t.Fatal("different seeds produced the same draws")
	}
	unseeded := &Generator{}
	if slices.Equal(draws(unseeded, "arrivals"), draws(unseeded, "arrivals")) {
		t.Fatal("an unseeded generator replayed its draws")
	}
}
