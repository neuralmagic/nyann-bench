package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func TestIsolatedStarlarkCompilerProcess(t *testing.T) {
	t.Setenv("NYANN_BENCH_COMPILER_HELPER", "1")
	t.Setenv("NYANN_BENCH_TEST_BINARY", os.Args[0])
	script := filepath.Join(t.TempDir(), "compiler")
	contents := "#!/bin/sh\nexec \"$NYANN_BENCH_TEST_BINARY\" -test.run=TestStarlarkCompilerHelperProcess\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	options := testOptions()
	options.StarlarkCompilerPath = script
	server := NewServer(newMCPClient(), "test", options)
	scenario, err := server.compileStarlark(context.Background(), `scenario(stages=[stage("1s", concurrency=7)])`)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenario.Stages) != 1 || scenario.Stages[0].Concurrency != 7 {
		t.Fatalf("isolated scenario = %+v", scenario)
	}
}

func TestStarlarkCompilerHelperProcess(t *testing.T) {
	if os.Getenv("NYANN_BENCH_COMPILER_HELPER") != "1" {
		return
	}
	source, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	scenario, err := config.ParseStarlarkSource("<helper>", string(source))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(scenario); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestStarlarkCompilerSerializesRequestsAndReleasesOnCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	options := testOptions()
	options.starlarkCompiler = func(ctx context.Context, source string) (*config.ScenarioConfig, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return config.ParseStarlarkSource("<test>", source)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	server := NewServer(newMCPClient(), "test", options)
	firstDone := make(chan error, 1)
	go func() {
		_, err := server.compileStarlark(context.Background(), `scenario(stages=[stage("1s")])`)
		firstDone <- err
	}()
	<-entered

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.compileStarlark(waitCtx, `scenario(stages=[stage("1s")])`); err == nil || !strings.Contains(err.Error(), "compiler capacity") {
		t.Fatalf("queued cancellation error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("queued request entered compiler; calls=%d", calls.Load())
	}

	release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	thirdDone := make(chan error, 1)
	go func() {
		_, err := server.compileStarlark(context.Background(), `scenario(stages=[stage("1s")])`)
		thirdDone <- err
	}()
	<-entered
	release <- struct{}{}
	if err := <-thirdDone; err != nil {
		t.Fatal(err)
	}
}
