package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Record is a single completed request record written to JSONL.
type Record struct {
	RequestID      string    `json:"id"`
	StreamID       int       `json:"stream"`
	ConversationID string    `json:"conv_id"`
	Turn           int       `json:"turn"`
	StartTime      float64   `json:"t0"`
	TTFT           float64   `json:"ttft_ms"`
	ITLs           []float64 `json:"itls_ms,omitempty"`
	EndTime        float64   `json:"tend"`
	PromptTokens   int       `json:"prompt_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	TotalLatencyMs float64   `json:"latency_ms"`
	FinishReason   string    `json:"finish_reason,omitempty"`
	Status         string    `json:"status"` // "ok" or "error"
	Error          string    `json:"error,omitempty"`

	// Eval fields (populated when dataset provides ExpectedAnswer)
	EvalExpected  string `json:"eval_expected,omitempty"`
	EvalExtracted string `json:"eval_extracted,omitempty"`
	EvalCorrect   *bool  `json:"eval_correct,omitempty"`
}

// written is a pre-marshalled record: the serialized JSONL line plus the
// parsed Record for the in-memory buffer.
type written struct {
	line   []byte
	record Record
}

// Recorder writes per-request records. Thread-safe.
//
// JSON marshalling happens in the caller goroutine (parallel), and a single
// background goroutine writes pre-serialized bytes to disk. This eliminates
// lock contention on the marshal step at high concurrency.
type Recorder struct {
	ch      chan written
	done    chan struct{}
	file    *os.File
	mu      sync.Mutex
	records []Record
}

// New creates a file-based recorder that also buffers in memory.
func New(dir string, workerID int) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("%s/requests_%d.jsonl", dir, workerID)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		ch:   make(chan written, 8192),
		done: make(chan struct{}),
		file: f,
	}
	go r.drain()
	return r, nil
}

// NewMemory creates an in-memory recorder with no file output.
func NewMemory() *Recorder {
	r := &Recorder{
		ch:   make(chan written, 8192),
		done: make(chan struct{}),
	}
	go r.drain()
	return r
}

func (r *Recorder) drain() {
	defer close(r.done)
	for w := range r.ch {
		r.mu.Lock()
		r.records = append(r.records, w.record)
		r.mu.Unlock()
		if r.file != nil {
			r.file.Write(w.line)
		}
	}
}

func (r *Recorder) Write(rec *Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	r.ch <- written{line: line, record: *rec}
	return nil
}

// Records returns all buffered records. Must be called after Close.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

// Close drains pending writes and closes the underlying file.
// Safe to call multiple times.
func (r *Recorder) Close() error {
	r.mu.Lock()
	ch := r.ch
	r.ch = nil
	r.mu.Unlock()

	if ch == nil {
		return nil // already closed
	}

	close(ch)
	<-r.done
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// Timestamps holds phase timestamps for a single worker.
type Timestamps struct {
	StartTime      float64 `json:"start_time"`
	RampupEndTime  float64 `json:"rampup_end_time"`
	EndTime        float64 `json:"end_time"`
	RampupSeconds  float64 `json:"rampup_duration_seconds"`
	TotalSeconds   float64 `json:"total_duration_seconds"`
}

func (t *Timestamps) Write(path string) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func TimeToFloat(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}
