package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateCLIParsesYAMLConfigBeforeWorkerValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workload.yaml")
	if err := os.WriteFile(path, []byte("load:\n  concurrency: 4\n  duration: 1s\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--config", path, "--workers", "invalid"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--workers must be") {
		t.Fatalf("expected post-config worker validation error, got %v", err)
	}
}

func TestGenerateCLIReportsYAMLParseErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workload.yml")
	if err := os.WriteFile(path, []byte("load:\n  duration: [not-a-duration]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := generateCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--config", path})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "config: parsing YAML config") {
		t.Fatalf("expected contextual YAML error, got %v", err)
	}
}
