package mockserver_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/mockserver"
)

func startTestServer(t *testing.T, ttft, itl time.Duration, outputTokens int) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:         addr,
		TTFT:         ttft,
		ITL:          itl,
		OutputTokens: outputTokens,
		Model:        "test-model",
	}

	go srv.ListenAndServe()

	// Wait for server to be ready
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return ""
}

func TestMockServerStreaming(t *testing.T) {
	addr := startTestServer(t, 10*time.Millisecond, 1*time.Millisecond, 10)
	c := client.New("http://" + addr + "/v1")

	req := &client.Request{
		Model: "test-model",
		Messages: []client.Message{
			{Role: "user", Content: "hello"},
		},
	}

	result := c.ChatStream(context.Background(), req)

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Content == "" {
		t.Fatal("expected non-empty content")
	}
	if len(result.TokenTimes) != 10 {
		t.Fatalf("expected 10 token events, got %d", len(result.TokenTimes))
	}
	if result.FirstToken.IsZero() {
		t.Fatal("expected non-zero first token time")
	}

	ttft := result.TTFT()
	if ttft < 5*time.Millisecond {
		t.Fatalf("TTFT too low: %v", ttft)
	}

	itls := result.ITLs()
	if len(itls) != 9 {
		t.Fatalf("expected 9 ITLs, got %d", len(itls))
	}
}

func TestMockServerTiming(t *testing.T) {
	ttft := 50 * time.Millisecond
	itl := 5 * time.Millisecond
	tokens := 20

	addr := startTestServer(t, ttft, itl, tokens)
	c := client.New("http://" + addr + "/v1")

	req := &client.Request{
		Model: "test-model",
		Messages: []client.Message{
			{Role: "user", Content: "hello"},
		},
	}

	result := c.ChatStream(context.Background(), req)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	// TTFT should be roughly 50ms (allow generous margin for CI)
	measuredTTFT := result.TTFT()
	if measuredTTFT < 30*time.Millisecond || measuredTTFT > 200*time.Millisecond {
		t.Errorf("TTFT out of range: %v (expected ~%v)", measuredTTFT, ttft)
	}

	// Total latency should be roughly TTFT + (tokens-1)*ITL
	expectedTotal := ttft + itl*time.Duration(tokens-1)
	totalLatency := result.TotalLatency()
	if totalLatency < expectedTotal/2 || totalLatency > expectedTotal*3 {
		t.Errorf("total latency out of range: %v (expected ~%v)", totalLatency, expectedTotal)
	}
}

func TestMockServerUsage(t *testing.T) {
	addr := startTestServer(t, 5*time.Millisecond, 1*time.Millisecond, 15)
	c := client.New("http://" + addr + "/v1")

	req := &client.Request{
		Model: "test-model",
		Messages: []client.Message{
			{Role: "user", Content: "hello"},
		},
	}

	result := c.ChatStream(context.Background(), req)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	if result.Usage == nil {
		t.Fatal("expected usage in response")
	}
	if result.Usage.CompletionTokens != 15 {
		t.Fatalf("expected 15 completion tokens, got %d", result.Usage.CompletionTokens)
	}
}
