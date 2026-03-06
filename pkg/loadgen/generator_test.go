package loadgen_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/loadgen"
	"github.com/neuralmagic/nyann_poker/pkg/mockserver"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
)

func startMockServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	srv := &mockserver.Server{
		Addr:         addr,
		TTFT:         5 * time.Millisecond,
		ITL:          1 * time.Millisecond,
		OutputTokens: 10,
		Model:        "test-model",
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
	t.Fatal("server did not start")
	return ""
}

func TestGeneratorBasic(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 2,
		Rampup:      0,
		Duration:    500 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(32, 10, 1),
		Recorder:    rec,
	}

	ts, err := gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	if ts.StartTime == 0 || ts.EndTime == 0 {
		t.Fatal("timestamps should be non-zero")
	}
	if ts.TotalSeconds < 0.4 || ts.TotalSeconds > 2.0 {
		t.Errorf("unexpected total duration: %f", ts.TotalSeconds)
	}

	// Check that records were written
	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected at least one record")
	}

	okCount := 0
	for _, r := range records {
		if r.Status == "ok" {
			okCount++
			if r.TTFT <= 0 {
				t.Errorf("record %s has zero TTFT", r.RequestID)
			}
		}
	}
	if okCount == 0 {
		t.Fatal("expected at least one successful record")
	}
}

func TestGeneratorMultiTurn(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 1,
		Duration:    1 * time.Second,
		Dataset:     dataset.NewSynthetic(32, 10, 3), // 3 turns
		Recorder:    rec,
	}

	_, err = gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))
	if len(records) == 0 {
		t.Fatal("expected records")
	}

	// Check that we have multi-turn conversations (turns 0, 1, 2)
	turnsSeen := map[int]bool{}
	for _, r := range records {
		turnsSeen[r.Turn] = true
	}
	if !turnsSeen[0] || !turnsSeen[1] || !turnsSeen[2] {
		t.Errorf("expected turns 0, 1, 2; got turns: %v", turnsSeen)
	}
}

func TestGeneratorRampup(t *testing.T) {
	addr := startMockServer(t)
	outDir := t.TempDir()

	rec, err := recorder.New(outDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	gen := &loadgen.Generator{
		Target:      "http://" + addr + "/v1",
		Model:       "test-model",
		Concurrency: 4,
		Rampup:      200 * time.Millisecond,
		Duration:    800 * time.Millisecond,
		Dataset:     dataset.NewSynthetic(16, 5, 1),
		Recorder:    rec,
	}

	ts, err := gen.Run(context.Background())
	if err != nil {
		t.Fatalf("generator run failed: %v", err)
	}

	// Rampup should be 200ms
	if ts.RampupSeconds < 0.15 || ts.RampupSeconds > 0.3 {
		t.Errorf("unexpected rampup duration: %f", ts.RampupSeconds)
	}

	// Check that first requests from different streams are staggered
	rec.Close()
	records := readRecords(t, filepath.Join(outDir, "requests_0.jsonl"))

	// Find first request per stream
	firstByStream := map[int]float64{}
	for _, r := range records {
		if _, ok := firstByStream[r.StreamID]; !ok {
			firstByStream[r.StreamID] = r.StartTime
		}
	}

	if len(firstByStream) < 2 {
		t.Skip("not enough streams completed to verify stagger")
	}

	// Streams should start at different times
	var minT, maxT float64
	first := true
	for _, st := range firstByStream {
		if first || st < minT {
			minT = st
		}
		if first || st > maxT {
			maxT = st
		}
		first = false
	}
	spread := maxT - minT
	if spread < 0.05 { // At least 50ms spread with 200ms rampup
		t.Errorf("streams not staggered enough: spread=%.1fms", spread*1000)
	}
}

func TestTimestampsWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timestamps.json")

	ts := &recorder.Timestamps{
		StartTime:     1000.0,
		RampupEndTime: 1060.0,
		EndTime:       1660.0,
		RampupSeconds: 60.0,
		TotalSeconds:  660.0,
	}

	if err := ts.Write(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var loaded recorder.Timestamps
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if loaded.StartTime != 1000.0 || loaded.RampupEndTime != 1060.0 || loaded.EndTime != 1660.0 {
		t.Errorf("timestamps mismatch: %+v", loaded)
	}
}

func readRecords(t *testing.T, path string) []recorder.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var records []recorder.Record
	dec := json.NewDecoder(strings.NewReader(string(data)))
	for dec.More() {
		var r recorder.Record
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}
