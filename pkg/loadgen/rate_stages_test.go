package loadgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// countingServer counts the requests it served.
func countingServer(t *testing.T) (string, func() int) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int { return int(n.Load()) }
}

func rateStageGenerator(t *testing.T, url string, mode loadgen.Mode, rate float64) (*loadgen.Generator, *recorder.Recorder, string) {
	t.Helper()
	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &loadgen.Generator{
		Target:   url + "/v1",
		Model:    "test-model",
		Mode:     mode,
		Rate:     rate,
		Dataset:  dataset.NewSynthetic(16, 4, 2, 4.0),
		Recorder: rec,
	}, rec, outDir
}

func TestRateBasedStagesDispatchAtTheConfiguredRate(t *testing.T) {
	for _, mode := range []loadgen.Mode{loadgen.ModePoisson, loadgen.ModeConstant} {
		t.Run(string(mode), func(t *testing.T) {
			url, served := countingServer(t)
			gen, rec, _ := rateStageGenerator(t, url, mode, 100)
			gen.RunStages(context.Background(), []loadgen.Stage{{Duration: 300 * time.Millisecond}}, nil, nil)
			rec.Close()

			if n := served(); n < 10 {
				t.Fatalf("server saw %d requests, want the stage rate to drive load", n)
			}
		})
	}
}

func TestRateBasedStagesRunEachStageForItsOwnDuration(t *testing.T) {
	url, served := countingServer(t)
	gen, rec, _ := rateStageGenerator(t, url, loadgen.ModeConstant, 50)

	var stageStarts []int
	start := time.Now()
	gen.RunStages(context.Background(), []loadgen.Stage{
		{Duration: 250 * time.Millisecond},
		{Duration: 250 * time.Millisecond},
	}, func(_, _ int) { stageStarts = append(stageStarts, served()) }, nil)
	elapsed := time.Since(start)
	rec.Close()

	if len(stageStarts) != 2 {
		t.Fatalf("onStage fired %d times, want once per stage", len(stageStarts))
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("run took %s, want at least the sum of the stage durations", elapsed)
	}
	if stageStarts[1] == 0 {
		t.Fatal("second stage started before the first stage sent anything")
	}
	if served() <= stageStarts[1] {
		t.Fatalf("second stage sent no requests (%d at start, %d total)", stageStarts[1], served())
	}
}

func TestRateBasedStagesRecordEveryRequest(t *testing.T) {
	for _, mode := range []loadgen.Mode{loadgen.ModePoisson, loadgen.ModeConstant} {
		t.Run(string(mode), func(t *testing.T) {
			for attempt := 0; attempt < 5; attempt++ {
				url, served := countingServer(t)
				gen, rec, outDir := rateStageGenerator(t, url, mode, 200)
				gen.RunStages(context.Background(), []loadgen.Stage{{Duration: 200 * time.Millisecond}}, nil, nil)
				rec.Close()

				sent := served()
				written := len(readRecords(t, filepath.Join(outDir, "requests_0.jsonl")))
				if sent == 0 || written != sent {
					t.Fatalf("attempt %d: server saw %d requests, %d records written", attempt, sent, written)
				}
			}
		})
	}
}

func TestRateBasedStagesRunBarrierCallbacks(t *testing.T) {
	url, _ := countingServer(t)
	gen, rec, _ := rateStageGenerator(t, url, loadgen.ModeConstant, 50)

	var barriers []int
	gen.RunStages(context.Background(), []loadgen.Stage{
		{Duration: 100 * time.Millisecond},
		{Barrier: true},
		{Duration: 100 * time.Millisecond},
	}, nil, func(i int) { barriers = append(barriers, i) })
	rec.Close()

	if len(barriers) != 1 || barriers[0] != 1 {
		t.Fatalf("barrier callbacks = %v, want [1]", barriers)
	}
}
