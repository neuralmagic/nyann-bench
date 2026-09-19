package loadgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// requestIDServer records the X-Request-Id of every chat request.
func requestIDServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("X-Request-Id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"text":"ok","finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ids...)
	}
}

func TestRequestIDNamesTheModelAndRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func(url string) *loadgen.Generator
	}{
		{"concurrent", func(url string) *loadgen.Generator {
			return &loadgen.Generator{
				Target: url + "/v1", Model: "adapter-3", Concurrency: 2,
				Duration: 300 * time.Millisecond,
				Dataset:  dataset.NewSynthetic(16, 4, 3, 4.0),
			}
		}},
		{"conversation_pool", func(url string) *loadgen.Generator {
			return &loadgen.Generator{
				Target: url + "/v1", Model: "adapter-3", Mode: loadgen.ModeConversationPool,
				Concurrency: 2, ConversationPoolSize: 4,
				Duration: 300 * time.Millisecond,
				Dataset:  dataset.NewSynthetic(16, 4, 3, 4.0),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, served := requestIDServer(t)
			gen := tc.gen(url)
			rec, err := recorder.New(t.TempDir(), 0)
			if err != nil {
				t.Fatal(err)
			}
			gen.Recorder = rec
			if _, err := gen.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec.Close()

			ids := served()
			if len(ids) == 0 {
				t.Fatal("no requests reached the server")
			}
			turns := map[string][]int{}
			seen := map[string]bool{}
			for _, id := range ids {
				model, rest, ok := strings.Cut(id, "|")
				if !ok {
					t.Fatalf("X-Request-Id %q has no model prefix", id)
				}
				if model != "adapter-3" {
					t.Fatalf("X-Request-Id %q names model %q, want adapter-3", id, model)
				}
				i := strings.LastIndex(rest, "-t")
				if i < 0 {
					t.Fatalf("X-Request-Id %q has no turn suffix", id)
				}
				conv := rest[:i]
				turn, err := strconv.Atoi(rest[i+2:])
				if err != nil {
					t.Fatalf("X-Request-Id %q: %v", id, err)
				}
				if seen[id] {
					t.Fatalf("X-Request-Id %q sent twice", id)
				}
				seen[id] = true
				turns[conv] = append(turns[conv], turn)
			}
			for conv, got := range turns {
				sort.Ints(got)
				for i, turn := range got {
					if turn != i {
						t.Fatalf("conversation %s turns = %v, want them numbered from zero", conv, got)
					}
				}
			}
		})
	}
}

// promptDataset serves single-turn conversations over the completions API.
type promptDataset struct{}

func (promptDataset) NextConversation() dataset.Conversation {
	return dataset.Conversation{Prompt: "a prompt", MaxTokens: 4}
}

func TestRequestIDStampsCompletionRequests(t *testing.T) {
	url, served := requestIDServer(t)
	rec, err := recorder.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	gen := &loadgen.Generator{
		Target:      url + "/v1",
		Model:       "adapter-3",
		Concurrency: 1,
		Duration:    200 * time.Millisecond,
		Dataset:     promptDataset{},
		Recorder:    rec,
	}
	if _, err := gen.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.Close()

	ids := served()
	if len(ids) == 0 {
		t.Fatal("no requests reached the server")
	}
	for i, id := range ids {
		if !strings.HasPrefix(id, "adapter-3|") || !strings.HasSuffix(id, "-t0") {
			t.Fatalf("completion request %d carried X-Request-Id %q", i, id)
		}
	}
}
