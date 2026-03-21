package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
)

// makeRecord builds a realistic Record with ITLs.
func makeRecord(id int) *Record {
	itls := make([]float64, 64)
	for i := range itls {
		itls[i] = float64(i) * 0.5
	}
	return &Record{
		RequestID:      fmt.Sprintf("w0-c%d-t0", id),
		StreamID:       id % 64,
		ConversationID: fmt.Sprintf("w0-c%d", id),
		Turn:           0,
		StartTime:      1700000000.0 + float64(id),
		TTFT:           25.3,
		ITLs:           itls,
		EndTime:        1700000001.0 + float64(id),
		PromptTokens:   128,
		OutputTokens:   64,
		TotalLatencyMs: 1000.5,
		FinishReason:   "stop",
		Status:         "ok",
	}
}

// --- Approach 1: Current (mutex + in-memory buffer + json.Encode under lock) ---

func BenchmarkRecorderCurrent(b *testing.B) {
	for _, concurrency := range []int{1, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("c%d", concurrency), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := New(dir, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer rec.Close()

			b.ResetTimer()
			var wg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				wg.Add(1)
				go func(base int) {
					defer wg.Done()
					for i := 0; i < b.N; i++ {
						rec.Write(makeRecord(base*b.N + i))
					}
				}(c)
			}
			wg.Wait()
			b.StopTimer()
			b.ReportMetric(float64(concurrency*b.N), "total_writes")
		})
	}
}

// --- Approach 2: Channel-based (writers push to channel, single goroutine drains) ---

type channelRecorder struct {
	ch      chan Record
	done    chan struct{}
	file    *os.File
	enc     *json.Encoder
	records []Record
}

func newChannelRecorder(dir string, bufSize int) (*channelRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("%s/requests_0.jsonl", dir)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	cr := &channelRecorder{
		ch:   make(chan Record, bufSize),
		done: make(chan struct{}),
		file: f,
		enc:  json.NewEncoder(f),
	}
	go cr.drain()
	return cr, nil
}

func (cr *channelRecorder) drain() {
	defer close(cr.done)
	for rec := range cr.ch {
		cr.records = append(cr.records, rec)
		cr.enc.Encode(&rec)
	}
}

func (cr *channelRecorder) Write(rec *Record) {
	cr.ch <- *rec
}

func (cr *channelRecorder) Close() {
	close(cr.ch)
	<-cr.done
	cr.file.Close()
}

func BenchmarkRecorderChannel(b *testing.B) {
	for _, concurrency := range []int{1, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("c%d", concurrency), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := newChannelRecorder(dir, 8192)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			var wg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				wg.Add(1)
				go func(base int) {
					defer wg.Done()
					for i := 0; i < b.N; i++ {
						rec.Write(makeRecord(base*b.N + i))
					}
				}(c)
			}
			wg.Wait()
			b.StopTimer()
			rec.Close()
			b.ReportMetric(float64(concurrency*b.N), "total_writes")
		})
	}
}

// --- Approach 3: Mutex but no in-memory buffer (file-only, read back at end) ---

type fileOnlyRecorder struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func newFileOnlyRecorder(dir string) (*fileOnlyRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("%s/requests_0.jsonl", dir)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &fileOnlyRecorder{file: f, enc: json.NewEncoder(f)}, nil
}

func (r *fileOnlyRecorder) Write(rec *Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enc.Encode(rec)
}

func (r *fileOnlyRecorder) Close() {
	r.file.Close()
}

func BenchmarkRecorderFileOnly(b *testing.B) {
	for _, concurrency := range []int{1, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("c%d", concurrency), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := newFileOnlyRecorder(dir)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			var wg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				wg.Add(1)
				go func(base int) {
					defer wg.Done()
					for i := 0; i < b.N; i++ {
						rec.Write(makeRecord(base*b.N + i))
					}
				}(c)
			}
			wg.Wait()
			b.StopTimer()
			rec.Close()
			b.ReportMetric(float64(concurrency*b.N), "total_writes")
		})
	}
}

// --- Approach 4: Channel + marshal in caller (pre-serialize, channel sends bytes) ---

type preMarshalRecorder struct {
	ch      chan []byte
	done    chan struct{}
	file    *os.File
	mu      sync.Mutex
	records []Record
}

func newPreMarshalRecorder(dir string, bufSize int) (*preMarshalRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("%s/requests_0.jsonl", dir)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	pr := &preMarshalRecorder{
		ch:   make(chan []byte, bufSize),
		done: make(chan struct{}),
		file: f,
	}
	go pr.drain()
	return pr, nil
}

func (pr *preMarshalRecorder) drain() {
	defer close(pr.done)
	for line := range pr.ch {
		pr.file.Write(line)
	}
}

func (pr *preMarshalRecorder) Write(rec *Record) {
	line, _ := json.Marshal(rec)
	line = append(line, '\n')
	pr.ch <- line
}

func (pr *preMarshalRecorder) Close() {
	close(pr.ch)
	<-pr.done
	pr.file.Close()
}

func BenchmarkRecorderPreMarshal(b *testing.B) {
	for _, concurrency := range []int{1, 64, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("c%d", concurrency), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := newPreMarshalRecorder(dir, 8192)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			var wg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				wg.Add(1)
				go func(base int) {
					defer wg.Done()
					for i := 0; i < b.N; i++ {
						rec.Write(makeRecord(base*b.N + i))
					}
				}(c)
			}
			wg.Wait()
			b.StopTimer()
			rec.Close()
			b.ReportMetric(float64(concurrency*b.N), "total_writes")
		})
	}
}
