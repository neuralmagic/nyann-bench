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
	Status         string    `json:"status"` // "ok" or "error"
	Error          string    `json:"error,omitempty"`

	// Eval fields (populated when dataset provides ExpectedAnswer)
	EvalExpected  string `json:"eval_expected,omitempty"`
	EvalExtracted string `json:"eval_extracted,omitempty"`
	EvalCorrect   *bool  `json:"eval_correct,omitempty"`
}

// Recorder writes per-request records. Thread-safe.
// Supports file-based (JSONL) and in-memory modes.
type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	enc     *json.Encoder
	records []Record // in-memory buffer (always populated)
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
	return &Recorder{file: f, enc: json.NewEncoder(f)}, nil
}

// NewMemory creates an in-memory recorder with no file output.
func NewMemory() *Recorder {
	return &Recorder{}
}

func (r *Recorder) Write(rec *Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, *rec)
	if r.enc != nil {
		return r.enc.Encode(rec)
	}
	return nil
}

// Records returns all buffered records.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

func (r *Recorder) Close() error {
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
