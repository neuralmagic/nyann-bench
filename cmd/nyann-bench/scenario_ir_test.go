package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func compiledScenarioIR(t *testing.T, source string) string {
	t.Helper()
	scenario, err := config.ParseStarlarkSource("<test>", source)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestGenerateCLIParsesScenarioIRBeforeWorkerValidation(t *testing.T) {
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--scenario-ir", compiledScenarioIR(t, `scenario(stages=[stage("1s", concurrency=2)])`), "--workers", "invalid"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--workers must be") {
		t.Fatalf("expected post-config worker validation error, got %v", err)
	}
	if !cmd.Flags().Lookup("scenario-ir").Hidden {
		t.Fatal("internal scenario IR flag is publicly visible")
	}
}

func TestGenerateCLIRejectsConfigWithScenarioIR(t *testing.T) {
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--config", `{}`, "--scenario-ir", compiledScenarioIR(t, `scenario(stages=[stage("1s")])`)})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "if any flags in the group") {
		t.Fatalf("expected mutually exclusive config error, got %v", err)
	}
}

func TestGenerateCLIExecutesCompiledScenarioEndToEnd(t *testing.T) {
	addr := startTestServer(t)
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{
		"--target", "http://" + addr + "/v1",
		"--model", "test-model",
		"--scenario-ir", compiledScenarioIR(t, `scenario(stages=[stage("10s", concurrency=1, max_requests=1)], workload=workload("faker", isl=8, osl=4))`),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}
