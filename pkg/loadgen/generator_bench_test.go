package loadgen_test

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/mockserver"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
)

// startFastMockServer starts a mock server with minimal latency to make
// inter-request overhead the dominant factor.
func startFastMockServer(tb testing.TB) string {
	tb.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:         addr,
		TTFT:         1 * time.Millisecond,
		ITL:          100 * time.Microsecond,
		OutputTokens: 5,
		Model:        "bench-model",
	}
	go srv.ListenAndServe()

	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatal("server did not start")
	return ""
}

// BenchmarkThroughput measures requests/sec at various concurrency levels.
// With async recordResult, throughput should scale closer to the theoretical
// maximum (limited by mock server response time, not recording overhead).
func BenchmarkThroughput(b *testing.B) {
	addr := startFastMockServer(b)

	for _, concurrency := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("c%d", concurrency), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := recorder.New(dir, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer rec.Close()

			gen := &loadgen.Generator{
				Target:      "http://" + addr + "/v1",
				Model:       "bench-model",
				Concurrency: concurrency,
				Duration:    time.Duration(b.N) * time.Hour, // we control iterations
				Dataset:     dataset.NewSynthetic(8, 5, 1, 4.0),
				Recorder:    rec,
			}

			// Run for a fixed wall-clock duration and count completed requests.
			const runDuration = 2 * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), runDuration)
			defer cancel()

			b.ResetTimer()
			gen.Run(ctx)
			b.StopTimer()

			records := rec.Records()
			b.ReportMetric(float64(len(records))/runDuration.Seconds(), "req/s")
			b.ReportMetric(float64(len(records)), "total_reqs")
		})
	}
}

// TestConcurrencyUtilization measures how well actual concurrency tracks the
// target. Samples InFlight() at 1ms intervals and reports mean/target ratio.
// A ratio close to 1.0 means the streams are fully utilized.
func TestConcurrencyUtilization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	addr := startFastMockServer(t)

	for _, concurrency := range []int{4, 16, 64} {
		t.Run(fmt.Sprintf("c%d", concurrency), func(t *testing.T) {
			rec := recorder.NewMemory()
			gen := &loadgen.Generator{
				Target:      "http://" + addr + "/v1",
				Model:       "bench-model",
				Concurrency: concurrency,
				Duration:    3 * time.Second,
				Dataset:     dataset.NewSynthetic(8, 5, 1, 4.0),
				Recorder:    rec,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Sample InFlight() in background
			var samples atomic.Int64
			var sum atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				// Skip the first 200ms to let streams ramp up
				time.Sleep(200 * time.Millisecond)
				ticker := time.NewTicker(1 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						sum.Add(gen.InFlight())
						samples.Add(1)
					}
				}
			}()

			gen.Run(ctx)
			cancel()
			<-done

			n := samples.Load()
			if n == 0 {
				t.Fatal("no samples collected")
			}
			mean := float64(sum.Load()) / float64(n)
			ratio := mean / float64(concurrency)

			records := rec.Records()
			t.Logf("target=%d  mean_inflight=%.1f  ratio=%.3f  completed=%d",
				concurrency, mean, ratio, len(records))

			if ratio < 0.80 {
				t.Errorf("concurrency utilization %.1f%% is below 80%% threshold", ratio*100)
			}
		})
	}
}
