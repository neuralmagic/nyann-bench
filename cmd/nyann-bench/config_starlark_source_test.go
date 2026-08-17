package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestGenerateCLIParsesInlineStarlarkBeforeWorkerValidation(t *testing.T) {
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--starlark-source", `scenario(stages=[stage("1s", concurrency=2)])`, "--workers", "invalid"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--workers must be") {
		t.Fatalf("expected post-config worker validation error, got %v", err)
	}
}

func TestGenerateCLIRejectsConfigWithInlineStarlark(t *testing.T) {
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--config", `{}`, "--starlark-source", `scenario(stages=[stage("1s")])`})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "if any flags in the group") {
		t.Fatalf("expected mutually exclusive config error, got %v", err)
	}
}

func TestGenerateCLIExecutesInlineStarlarkEndToEnd(t *testing.T) {
	addr := startTestServer(t)
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{
		"--target", "http://" + addr + "/v1",
		"--model", "test-model",
		"--starlark-source", `scenario(stages=[stage("10s", concurrency=1, max_requests=1)], workload=workload("faker", isl=8, osl=4))`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(cmd.Flags().Lookup("starlark-source").Value); !strings.Contains(got, "max_requests=1") {
		t.Fatalf("inline Starlark flag was not retained: %q", got)
	}
}
