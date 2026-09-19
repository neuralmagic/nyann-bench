package loadgen_test

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// seededWorkload returns session start offsets and per-request think gaps.
func seededWorkload(t *testing.T, seed int64) ([]float64, map[string]float64) {
	t.Helper()
	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	gen := &loadgen.Generator{
		Target:    replyServer(t) + "/v1",
		Model:     "test-model",
		Mode:      loadgen.ModePoisson,
		Rate:      20,
		Dataset:   dataset.NewSynthetic(16, 4, 3, 4.0),
		Recorder:  rec,
		Seed:      seed,
		ThinkTime: &config.ThinkTime{Median: config.Duration(40 * time.Millisecond), Sigma: 0.8},
	}
	gen.RunStages(context.Background(), []loadgen.Stage{{Duration: 800 * time.Millisecond}}, nil, nil)
	rec.Close()

	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	end := map[string]float64{}
	var starts []float64
	for _, r := range records {
		end[r.RequestID] = r.EndTime
		if r.Turn == 0 {
			starts = append(starts, r.StartTime)
		}
	}
	sort.Float64s(starts)
	base := starts[0]
	for i := range starts {
		starts[i] -= base
	}
	gaps := map[string]float64{}
	for _, r := range records {
		if r.Turn == 0 {
			continue
		}
		if prev, ok := end[fmt.Sprintf("%s-t%d", r.ConversationID, r.Turn-1)]; ok {
			gaps[r.RequestID] = r.StartTime - prev
		}
	}
	return starts, gaps
}

func TestSeedReplaysArrivalsAndThinkTimes(t *testing.T) {
	firstStarts, firstGaps := seededWorkload(t, 7)
	secondStarts, secondGaps := seededWorkload(t, 7)

	if len(firstStarts) < 3 {
		t.Fatalf("only %d sessions started; too few to compare", len(firstStarts))
	}
	if len(firstStarts) != len(secondStarts) {
		t.Fatalf("seed 7 started %d sessions then %d", len(firstStarts), len(secondStarts))
	}
	for i := range firstStarts {
		if math.Abs(firstStarts[i]-secondStarts[i]) > 0.15 {
			t.Fatalf("session %d started at %.3fs then %.3fs", i, firstStarts[i], secondStarts[i])
		}
	}
	if len(firstGaps) == 0 {
		t.Fatal("no second turns ran, so think times were not compared")
	}
	var compared int
	for id, gap := range firstGaps {
		other, ok := secondGaps[id]
		if !ok {
			continue
		}
		if math.Abs(gap-other) > 0.05 {
			t.Fatalf("%s paused %.3fs then %.3fs", id, gap, other)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("the two runs shared no request ids")
	}
}

func TestDifferentSeedsProduceDifferentArrivals(t *testing.T) {
	first, _ := seededWorkload(t, 7)
	second, _ := seededWorkload(t, 8)

	if len(first) != len(second) {
		return // counts already differ
	}
	for i := range first {
		if math.Abs(first[i]-second[i]) > 0.05 {
			return
		}
	}
	t.Fatalf("seeds 7 and 8 produced the same arrival offsets: %v", first)
}
