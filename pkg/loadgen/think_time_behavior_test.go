package loadgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// replyServer replies with one token.
func replyServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runForRecords(t *testing.T, gen *loadgen.Generator) []recorder.Record {
	t.Helper()
	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	gen.Recorder = rec
	if _, err := gen.Run(context.Background()); err != nil {
		t.Fatalf("generator run failed: %v", err)
	}
	rec.Close()
	return readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
}

func TestThinkTimeSeparatesTurnsOnly(t *testing.T) {
	const pause = 150 * time.Millisecond
	records := runForRecords(t, &loadgen.Generator{
		Target:      replyServer(t) + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    1500 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(16, 4, 3, 4.0),
		ThinkTime:   &config.ThinkTime{Median: config.Duration(pause)},
	})

	byID := map[string]recorder.Record{}
	for _, rec := range records {
		byID[rec.RequestID] = rec
	}
	var turnGaps, convGaps int
	for _, rec := range records {
		if rec.Turn > 0 {
			prev, ok := byID[fmt.Sprintf("%s-t%d", rec.ConversationID, rec.Turn-1)]
			if !ok {
				t.Fatalf("record %s has no previous turn", rec.RequestID)
			}
			gap := time.Duration((rec.StartTime - prev.EndTime) * float64(time.Second))
			if gap < pause {
				t.Fatalf("record %s started %s after the previous turn, want >= %s", rec.RequestID, gap, pause)
			}
			turnGaps++
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(rec.ConversationID, "w0-c%d", &idx); err != nil {
			t.Fatalf("unexpected conversation id %q: %v", rec.ConversationID, err)
		}
		if idx == 0 {
			continue
		}
		last, ok := byID[fmt.Sprintf("w0-c%d-t2", idx-1)]
		if !ok {
			t.Fatalf("conversation w0-c%d has no last turn", idx-1)
		}
		gap := time.Duration((rec.StartTime - last.EndTime) * float64(time.Second))
		if gap >= pause {
			t.Fatalf("conversation %s started %s after the previous one, want no think time", rec.ConversationID, gap)
		}
		convGaps++
	}
	if turnGaps < 2 || convGaps < 1 {
		t.Fatalf("expected gaps between turns and between conversations, got %d and %d", turnGaps, convGaps)
	}
}

func TestThinkTimeEndsWithRun(t *testing.T) {
	start := time.Now()
	records := runForRecords(t, &loadgen.Generator{
		Target:      replyServer(t) + "/v1",
		Model:       "test-model",
		Concurrency: 2,
		Duration:    300 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(16, 4, 2, 4.0),
		ThinkTime:   &config.ThinkTime{Median: config.Duration(10 * time.Second)},
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("run took %s; think time did not end with the run", elapsed)
	}
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for _, rec := range records {
		if rec.Turn != 0 {
			t.Fatalf("record %s: a turn after a 10s think time ran inside a 300ms run", rec.RequestID)
		}
	}
}

func TestThinkTimeHoldsTheInflightSlotInRateModes(t *testing.T) {
	const pause = 200 * time.Millisecond
	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	gen := &loadgen.Generator{
		Target:      replyServer(t) + "/v1",
		Model:       "test-model",
		Mode:        loadgen.ModeConstant,
		Rate:        50,
		MaxInFlight: 2,
		Dataset:     dataset.NewSynthetic(16, 4, 3, 4.0),
		Recorder:    rec,
		ThinkTime:   &config.ThinkTime{Median: config.Duration(pause)},
	}
	gen.RunStages(context.Background(), []loadgen.Stage{{Duration: 700 * time.Millisecond}}, nil, nil)
	rec.Close()

	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	convs := map[string]int{}
	for _, r := range records {
		convs[r.ConversationID]++
	}
	var completed int
	for _, turns := range convs {
		if turns == 3 {
			completed++
		}
	}
	if completed == 0 {
		t.Fatalf("no session finished all three turns across %d conversations", len(convs))
	}
	if len(convs) > 8 {
		t.Fatalf("%d sessions ran with max_inflight 2 and a %s pause per turn", len(convs), pause)
	}
}
