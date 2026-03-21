package recorder

import (
	"fmt"
	"sync"
	"testing"
)

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

func BenchmarkRecorder(b *testing.B) {
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
