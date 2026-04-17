package barrier

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestAllWorkersJoin(t *testing.T) {
	const nWorkers = 5
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := NewServer(nWorkers, 0) // port 0 = pick a free port
	port := startTestServer(t, ctx, srv)
	addr := addrWithPort(port)

	var (
		mu     sync.Mutex
		times  []time.Time
		wg     sync.WaitGroup
		errCh  = make(chan error, nWorkers)
	)

	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			st, err := WaitForStart(ctx, addr, workerID, 0, nWorkers, 10*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			times = append(times, st)
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("worker error: %v", err)
	}

	if len(times) != nWorkers {
		t.Fatalf("expected %d times, got %d", nWorkers, len(times))
	}

	// All workers must receive the same start time
	for i := 1; i < len(times); i++ {
		if !times[i].Equal(times[0]) {
			t.Errorf("worker %d got start_time %v, want %v", i, times[i], times[0])
		}
	}

	// Start time should be in the future (or very near past by now)
	if times[0].Before(time.Now().Add(-2 * time.Second)) {
		t.Errorf("start_time %v is too far in the past", times[0])
	}
}

func TestMultipleBarriers(t *testing.T) {
	const nWorkers = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := NewServer(nWorkers, 0)
	port := startTestServer(t, ctx, srv)
	addr := addrWithPort(port)

	// Run two barriers in sequence
	for barrierID := 0; barrierID < 2; barrierID++ {
		var (
			mu    sync.Mutex
			times []time.Time
			wg    sync.WaitGroup
			errCh = make(chan error, nWorkers)
		)

		for i := 0; i < nWorkers; i++ {
			wg.Add(1)
			go func(workerID, bid int) {
				defer wg.Done()
				st, err := WaitForStart(ctx, addr, workerID, bid, nWorkers, 10*time.Second)
				if err != nil {
					errCh <- err
					return
				}
				mu.Lock()
				times = append(times, st)
				mu.Unlock()
			}(i, barrierID)
		}

		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Fatalf("barrier %d worker error: %v", barrierID, err)
		}

		if len(times) != nWorkers {
			t.Fatalf("barrier %d: expected %d times, got %d", barrierID, nWorkers, len(times))
		}

		for i := 1; i < len(times); i++ {
			if !times[i].Equal(times[0]) {
				t.Errorf("barrier %d: worker %d got different start_time", barrierID, i)
			}
		}
	}
}

func TestTimeout(t *testing.T) {
	const nWorkers = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := NewServer(nWorkers, 0)
	port := startTestServer(t, ctx, srv)
	addr := addrWithPort(port)

	// Only send 2 of 3 workers — the third never arrives
	_, err := WaitForStart(ctx, addr, 0, 0, nWorkers, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestDuplicateWorkerID(t *testing.T) {
	const nWorkers = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := NewServer(nWorkers, 0)
	port := startTestServer(t, ctx, srv)
	addr := addrWithPort(port)

	// First registration should block (not all workers ready)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		WaitForStart(ctx, addr, 0, 0, nWorkers, 5*time.Second)
	}()

	// Give the first request time to register
	time.Sleep(100 * time.Millisecond)

	// Duplicate worker ID should fail
	_, err := WaitForStart(ctx, addr, 0, 0, nWorkers, 1*time.Second)
	if err == nil {
		t.Fatal("expected error for duplicate worker_id, got nil")
	}

	cancel() // clean up blocked goroutine
	wg.Wait()
}

func TestSingleWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Single worker should return immediately without a server
	st, err := WaitForStart(ctx, "unused:8080", 0, 0, 1, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(st) > 2*time.Second {
		t.Error("start time is too far in the past for single worker")
	}
}

func TestContextCancel(t *testing.T) {
	const nWorkers = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := NewServer(nWorkers, 0)
	port := startTestServer(t, ctx, srv)
	addr := addrWithPort(port)

	// Cancel context while worker is waiting
	waitCtx, waitCancel := context.WithCancel(ctx)

	errCh := make(chan error, 1)
	go func() {
		_, err := WaitForStart(waitCtx, addr, 0, 0, nWorkers, 30*time.Second)
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond)
	waitCancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
}

// startTestServer starts a barrier server on a random port and returns the port.
func startTestServer(t *testing.T, ctx context.Context, srv *Server) int {
	t.Helper()

	// Use port 0 to get a random free port
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	srv.port = port
	go func() {
		if err := srv.ListenAndServe(ctx); err != nil {
			// Ignore errors after context cancel
			if ctx.Err() == nil {
				t.Errorf("barrier server error: %v", err)
			}
		}
	}()

	// Wait for server to be ready
	time.Sleep(50 * time.Millisecond)
	return port
}

func addrWithPort(port int) string {
	return fmt.Sprintf("localhost:%d", port)
}
