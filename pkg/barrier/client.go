package barrier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// WaitForStart sends a ready signal to the barrier server and blocks until
// all workers have arrived. Returns the negotiated start time that all pods
// should sleep until before beginning the next phase.
//
// The function retries connection errors with exponential backoff, since the
// leader pod's barrier server may not be listening yet when workers reach
// this point.
func WaitForStart(ctx context.Context, addr string, workerID, barrierID, nWorkers int, timeout time.Duration) (time.Time, error) {
	if nWorkers <= 1 {
		// Single pod — no sync needed, start immediately
		return time.Now(), nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://%s/barrier/ready", addr)
	body, _ := json.Marshal(readyRequest{
		WorkerID:  workerID,
		BarrierID: barrierID,
	})

	slog.Info("Waiting at barrier", "barrier_id", barrierID, "worker_id", workerID, "addr", addr)

	// Retry loop for connection errors (leader may not be up yet)
	backoff := 100 * time.Millisecond
	maxBackoff := 5 * time.Second
	for {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return time.Time{}, fmt.Errorf("barrier: creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Connection error — retry with backoff
			if ctx.Err() != nil {
				return time.Time{}, fmt.Errorf("barrier: timed out waiting for barrier server at %s: %w", addr, ctx.Err())
			}
			slog.Debug("Barrier server not ready, retrying", "error", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return time.Time{}, fmt.Errorf("barrier: timed out waiting for barrier server at %s: %w", addr, ctx.Err())
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			return time.Time{}, fmt.Errorf("barrier: server returned %d: %s", resp.StatusCode, string(respBody))
		}

		var result readyResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return time.Time{}, fmt.Errorf("barrier: decoding response: %w", err)
		}

		startTime, err := time.Parse(time.RFC3339Nano, result.StartTime)
		if err != nil {
			return time.Time{}, fmt.Errorf("barrier: parsing start_time: %w", err)
		}

		slog.Info("Barrier released", "barrier_id", barrierID, "start_time", startTime.Format(time.RFC3339Nano))
		return startTime, nil
	}
}
