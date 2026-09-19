package loadgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// headerServer streams a one-token reply with two response headers.
func headerServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Host", "10.0.0.7:8000")
		w.Header().Set("X-Other", "unrecorded")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runWithRecordHeaders(t *testing.T, names []string) []recorder.Record {
	t.Helper()
	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	gen := &loadgen.Generator{
		Target:        headerServer(t) + "/v1",
		Model:         "test-model",
		Concurrency:   1,
		Duration:      200 * time.Millisecond,
		Dataset:       dataset.NewSynthetic(16, 4, 1, 4.0),
		Recorder:      rec,
		RecordHeaders: names,
	}
	if _, err := gen.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.Close()
	return readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
}

func TestRecordsNamedResponseHeaders(t *testing.T) {
	records := runWithRecordHeaders(t, []string{"X-Upstream-Host"})
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for i, r := range records {
		if r.Headers["X-Upstream-Host"] != "10.0.0.7:8000" {
			t.Fatalf("record %d: Headers = %v, want X-Upstream-Host", i, r.Headers)
		}
		if _, ok := r.Headers["X-Other"]; ok {
			t.Fatalf("record %d: recorded an unnamed header: %v", i, r.Headers)
		}
	}
}

func TestNoRecordedHeadersByDefault(t *testing.T) {
	records := runWithRecordHeaders(t, nil)
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for i, r := range records {
		if r.Headers != nil {
			t.Fatalf("record %d: Headers = %v, want nil", i, r.Headers)
		}
	}
}

func TestRecordHeaderNamesAreCaseInsensitive(t *testing.T) {
	records := runWithRecordHeaders(t, []string{"x-upstream-host"})
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for i, r := range records {
		if r.Headers["x-upstream-host"] != "10.0.0.7:8000" {
			t.Fatalf("record %d: Headers = %v, want the name as configured", i, r.Headers)
		}
	}
}
