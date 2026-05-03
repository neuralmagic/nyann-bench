package barrier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	dnsFailures := 0
	attempt := 0
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return time.Time{}, fmt.Errorf("barrier: creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return time.Time{}, fmt.Errorf("barrier: timed out waiting for barrier server at %s after %d attempts: %w", addr, attempt, ctx.Err())
			}

			// DNS NXDOMAIN — hostname doesn't exist, retrying won't help
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				dnsFailures++
				if dnsFailures >= 3 {
					host := addr
					if h, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
						host = h
					}
					return time.Time{}, fmt.Errorf("barrier: DNS lookup failed for %q — "+
						"the hostname does not resolve. For Kubernetes Indexed Jobs, BARRIER_ADDR must "+
						"include the headless service name (e.g. <job>-0.<service>): %w", host, err)
				}
				slog.Warn("Barrier DNS lookup failed, retrying", "addr", addr, "attempt", dnsFailures, "max_attempts", 3, "error", err)
			} else if attempt <= 3 {
				slog.Debug("Barrier server not ready, retrying", "error", err, "backoff", backoff)
			} else {
				slog.Warn("Barrier server not reachable", "addr", addr, "attempt", attempt, "error", err, "backoff", backoff)
			}

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return time.Time{}, fmt.Errorf("barrier: timed out waiting for barrier server at %s after %d attempts: %w", addr, attempt, ctx.Err())
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

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
