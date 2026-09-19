package loadgen_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/loadgen"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

const sessionHeader = "X-Session-Id"

type seenRequest struct {
	requestID string
	sessionID string
	hasHeader bool
}

// sessionServer records the request id and session header of every request.
func sessionServer(t *testing.T) (string, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasHeader := r.Header[http.CanonicalHeaderKey(sessionHeader)]
		mu.Lock()
		seen = append(seen, seenRequest{
			requestID: r.Header.Get("X-Request-Id"),
			sessionID: r.Header.Get(sessionHeader),
			hasHeader: hasHeader,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"ok"},"text":"ok","finish_reason":"stop"}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

// convOf extracts the conversation id from "<model>|<conv>-t<turn>".
func convOf(t *testing.T, requestID string) string {
	t.Helper()
	_, rest, ok := strings.Cut(requestID, "|")
	if !ok {
		t.Fatalf("malformed X-Request-Id %q", requestID)
	}
	i := strings.LastIndex(rest, "-t")
	if i < 0 {
		t.Fatalf("malformed X-Request-Id %q", requestID)
	}
	return rest[:i]
}

func runSessionGenerator(t *testing.T, gen *loadgen.Generator) []recorder.Record {
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

func TestSessionHeaderStablePerConversation(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func(url string) *loadgen.Generator
	}{
		{"concurrent", func(url string) *loadgen.Generator {
			return &loadgen.Generator{
				Target: url + "/v1", Model: "test-model", Concurrency: 2,
				Duration: 400 * time.Millisecond, SessionHeader: sessionHeader,
				Dataset: dataset.NewSynthetic(16, 4, 3, 4.0),
			}
		}},
		{"poisson", func(url string) *loadgen.Generator {
			return &loadgen.Generator{
				Target: url + "/v1", Model: "test-model", Mode: loadgen.ModePoisson, Rate: 30,
				Duration: 400 * time.Millisecond, SessionHeader: sessionHeader,
				Dataset: dataset.NewSynthetic(16, 4, 3, 4.0),
			}
		}},
		{"conversation_pool", func(url string) *loadgen.Generator {
			return &loadgen.Generator{
				Target: url + "/v1", Model: "test-model", Mode: loadgen.ModeConversationPool,
				Concurrency: 2, ConversationPoolSize: 4,
				Duration: 400 * time.Millisecond, SessionHeader: sessionHeader,
				Dataset: dataset.NewSynthetic(16, 4, 3, 4.0),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, requests := sessionServer(t)
			records := runSessionGenerator(t, tc.gen(url))

			seen := requests()
			if len(seen) == 0 {
				t.Fatal("no requests reached the server")
			}
			byConv := map[string]string{}
			sessions := map[string]bool{}
			var multiTurn bool
			for _, r := range seen {
				if r.sessionID == "" {
					t.Fatalf("request %q carried an empty session id", r.requestID)
				}
				if len(r.sessionID) != 32 {
					t.Fatalf("session id %q is not a 16-byte hex value", r.sessionID)
				}
				conv := convOf(t, r.requestID)
				if prev, ok := byConv[conv]; ok {
					multiTurn = true
					if prev != r.sessionID {
						t.Fatalf("conversation %s used session ids %q and %q", conv, prev, r.sessionID)
					}
				}
				byConv[conv] = r.sessionID
				sessions[r.sessionID] = true
			}
			if !multiTurn {
				t.Fatal("no conversation sent more than one turn, so stability was not exercised")
			}
			if len(sessions) < 2 {
				t.Fatalf("saw %d distinct session ids, want one per conversation", len(sessions))
			}

			if len(records) == 0 {
				t.Fatal("no records written")
			}
			for _, rec := range records {
				if !sessions[rec.SessionID] {
					t.Fatalf("record %s has session id %q, which no request sent", rec.RequestID, rec.SessionID)
				}
			}
		})
	}
}

func TestNoSessionHeaderByDefault(t *testing.T) {
	url, requests := sessionServer(t)
	records := runSessionGenerator(t, &loadgen.Generator{
		Target: url + "/v1", Model: "test-model", Concurrency: 1,
		Duration: 200 * time.Millisecond,
		Dataset:  dataset.NewSynthetic(16, 4, 2, 4.0),
	})

	seen := requests()
	if len(seen) == 0 {
		t.Fatal("no requests reached the server")
	}
	for _, r := range seen {
		if r.hasHeader {
			t.Fatalf("request %q sent a session header with none configured", r.requestID)
		}
	}
	for _, rec := range records {
		if rec.SessionID != "" {
			t.Fatalf("record %s has session id %q, want empty", rec.RequestID, rec.SessionID)
		}
	}
}

func TestSessionHeaderOnCompletionRequests(t *testing.T) {
	url, requests := sessionServer(t)
	records := runSessionGenerator(t, &loadgen.Generator{
		Target: url + "/v1", Model: "test-model", Concurrency: 1,
		Duration: 200 * time.Millisecond, SessionHeader: sessionHeader,
		Dataset: promptDataset{},
	})

	seen := requests()
	if len(seen) == 0 {
		t.Fatal("no requests reached the server")
	}
	sessions := map[string]bool{}
	for _, r := range seen {
		if len(r.sessionID) != 32 {
			t.Fatalf("completion request %q carried session id %q", r.requestID, r.sessionID)
		}
		sessions[r.sessionID] = true
	}
	if len(sessions) != len(seen) {
		t.Fatalf("%d single-turn completions shared %d session ids", len(seen), len(sessions))
	}
	if len(records) == 0 {
		t.Fatal("no records written")
	}
	for _, rec := range records {
		if !sessions[rec.SessionID] {
			t.Fatalf("record %s has session id %q, which no request sent", rec.RequestID, rec.SessionID)
		}
	}
}

// sessionIDsByConversation runs one workload at the given seed against a fresh
// server and maps each conversation id to the session id it sent.
func sessionIDsByConversation(t *testing.T, seed int64) map[string]string {
	t.Helper()
	url, requests := sessionServer(t)
	runSessionGenerator(t, &loadgen.Generator{
		Target: url + "/v1", Model: "test-model", Concurrency: 1,
		Duration: 300 * time.Millisecond, SessionHeader: sessionHeader, Seed: seed,
		Dataset: dataset.NewSynthetic(16, 4, 2, 4.0),
	})
	byConv := map[string]string{}
	for _, r := range requests() {
		byConv[convOf(t, r.requestID)] = r.sessionID
	}
	if len(byConv) < 2 {
		t.Fatalf("only %d conversations ran", len(byConv))
	}
	return byConv
}

// sharedConversations returns the conversation ids both runs reached.
func sharedConversations(t *testing.T, a, b map[string]string) []string {
	t.Helper()
	var shared []string
	for conv := range a {
		if _, ok := b[conv]; ok {
			shared = append(shared, conv)
		}
	}
	if len(shared) < 2 {
		t.Fatalf("the runs shared only %d conversations", len(shared))
	}
	return shared
}

func TestSeededSessionIDsReplay(t *testing.T) {
	first := sessionIDsByConversation(t, 11)
	second := sessionIDsByConversation(t, 11)
	for _, conv := range sharedConversations(t, first, second) {
		if first[conv] != second[conv] {
			t.Fatalf("conversation %s got session ids %q and %q at the same seed", conv, first[conv], second[conv])
		}
	}

	other := sessionIDsByConversation(t, 12)
	var differs bool
	for _, conv := range sharedConversations(t, first, other) {
		if first[conv] != other[conv] {
			differs = true
		}
	}
	if !differs {
		t.Fatal("seeds 11 and 12 produced the same session ids")
	}
}

func TestUnseededSessionIDsDoNotReplay(t *testing.T) {
	first := sessionIDsByConversation(t, 0)
	second := sessionIDsByConversation(t, 0)
	for _, conv := range sharedConversations(t, first, second) {
		if first[conv] == second[conv] {
			t.Fatalf("unseeded runs reused session id %q for conversation %s", first[conv], conv)
		}
	}
}
