package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func TestCompileStarlarkCommandEmitsBoundedScenarioIR(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("race runtime cannot start below the compiler subprocess memory limit")
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCompileStarlarkHelperProcess$")
	command.Env = append(os.Environ(), "NYANN_BENCH_COMPILE_STARLARK_HELPER=1")
	command.Stdin = bytes.NewBufferString(`scenario(stages=[stage("1s", concurrency=3)])`)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compiler helper: %v: %s", err, stderr.String())
	}
	scenario, err := config.ParseScenarioIR(string(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenario.Stages) != 1 || scenario.Stages[0].Concurrency != 3 {
		t.Fatalf("compiled scenario = %+v", scenario)
	}
}

func TestCompileStarlarkHelperProcess(t *testing.T) {
	if os.Getenv("NYANN_BENCH_COMPILE_STARLARK_HELPER") != "1" {
		return
	}
	command := compileStarlarkCmd()
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetIn(os.Stdin)
	command.SetOut(os.Stdout)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}
