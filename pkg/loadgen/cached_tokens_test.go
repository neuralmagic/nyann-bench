package loadgen_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// runOneTurnAgainst runs one short conversation against a server streaming usage.
func runOneTurnAgainst(t *testing.T, usage string) ([]recorder.Record, []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		if usage != "" {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\n", usage)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	outDir := t.TempDir()
	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	gen := &loadgen.Generator{
		Target:      srv.URL + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    200 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(16, 4, 1, 4.0),
		Recorder:    rec,
		StreamUsage: true,
	}
	if _, err := gen.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	path := filepath.Join(outDir, "requests_0.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return readRecords(t, path), raw
}

func TestRecordsCachedTokensFromServerUsage(t *testing.T) {
	records, _ := runOneTurnAgainst(t, `{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21,"prompt_tokens_details":{"cached_tokens":7}}`)
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for i, r := range records {
		if r.CachedTokens == nil {
			t.Fatalf("record %d: CachedTokens = nil, want 7", i)
		}
		if *r.CachedTokens != 7 {
			t.Fatalf("record %d: CachedTokens = %d, want 7", i, *r.CachedTokens)
		}
		if r.PromptTokens != 20 {
			t.Fatalf("record %d: PromptTokens = %d, want 20", i, r.PromptTokens)
		}
	}
}

func TestRecordOmitsCachedTokensWhenUnreported(t *testing.T) {
	records, raw := runOneTurnAgainst(t, `{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21}`)
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for i, r := range records {
		if r.CachedTokens != nil {
			t.Fatalf("record %d: CachedTokens = %d, want nil", i, *r.CachedTokens)
		}
	}
	if strings.Contains(string(raw), "cached_tokens") {
		t.Fatalf("cached_tokens present in JSONL when the server did not report it: %s", raw)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]), &row); err != nil {
		t.Fatal(err)
	}
	if _, ok := row["cached_tokens"]; ok {
		t.Fatal("cached_tokens key present with no server report")
	}
}
