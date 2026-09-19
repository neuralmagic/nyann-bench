package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

// promptTree writes a config file and a prompt file in the same directory and
// returns the directory and the config's path.
func promptTree(t *testing.T, configName, configBody, promptRel, promptBody string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if promptRel != "" {
		path := filepath.Join(dir, promptRel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(promptBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, configName)
	if err := os.WriteFile(cfg, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

func TestSystemPromptFileLoadsBesideTheConfig(t *testing.T) {
	const prompt = "You are a coding agent. Tools: read_file, patch, shell."
	for _, tc := range []struct {
		name   string
		file   string
		body   string
		prompt string
	}{
		{
			"starlark", "scenario.star",
			"scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=\"preamble.txt\"),\n)\n",
			"preamble.txt",
		},
		{
			"yaml", "config.yaml",
			"load:\n  concurrency: 1\n  duration: 1s\nworkload:\n  type: faker\n  system_prompt_file: preamble.txt\n",
			"preamble.txt",
		},
		{
			"json", "config.json",
			`{"load":{"concurrency":1,"duration":"1s"},"workload":{"type":"faker","system_prompt_file":"preamble.txt"}}`,
			"preamble.txt",
		},
		{
			"subdirectory", "scenario.star",
			"scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=\"prompts/agent.txt\"),\n)\n",
			"prompts/agent.txt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg := promptTree(t, tc.file, tc.body, tc.prompt, prompt+"\n")
			sc, err := config.Parse(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Workload.SystemPrompt != prompt {
				t.Fatalf("SystemPrompt = %q, want %q", sc.Workload.SystemPrompt, prompt)
			}
			// The path is consumed, so a compiled scenario carries the text.
			if sc.Workload.SystemPromptFile != "" {
				t.Fatalf("SystemPromptFile = %q, want it cleared", sc.Workload.SystemPromptFile)
			}
		})
	}
}

func TestSystemPromptFileOnAStageWorkload(t *testing.T) {
	body := "scenario(\n" +
		"    stages = [stage(\"60s\", workload=workload(\"faker\", system_prompt_file=\"stage.txt\"))],\n" +
		"    workload = workload(\"faker\"),\n" +
		")\n"
	_, cfg := promptTree(t, "scenario.star", body, "stage.txt", "stage prompt\n")
	sc, err := config.Parse(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Stages[0].Workload == nil || sc.Stages[0].Workload.SystemPrompt != "stage prompt" {
		t.Fatalf("stage workload = %+v", sc.Stages[0].Workload)
	}
}

func TestSystemPromptFileStaysInTheConfigDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"parent", "../secret.txt", "outside"},
		{"nested parent", "prompts/../../secret.txt", "outside"},
		{"absolute", "/etc/hostname", "must be a relative path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=" + quote(tc.path) + "),\n)\n"
			dir, cfg := promptTree(t, "scenario.star", body, "", "")
			// A readable file really does sit one level up.
			if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("secret"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := config.Parse(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestSystemPromptFileRejectsEscapingSymlink(t *testing.T) {
	body := "scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=\"link.txt\"),\n)\n"
	dir, cfg := promptTree(t, "scenario.star", body, "", "")
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := config.Parse(cfg)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error = %v, want the symlink target rejected", err)
	}
}

func TestSystemPromptFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arg    string
		prompt string
		want   string
	}{
		{"missing file", `system_prompt_file="absent.txt"`, "", "absent.txt"},
		{"directory", `system_prompt_file="prompts"`, "prompts/agent.txt", "not a regular file"},
		{"both options", `system_prompt="inline", system_prompt_file="preamble.txt"`, "preamble.txt", "mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", " + tc.arg + "),\n)\n"
			_, cfg := promptTree(t, "scenario.star", body, tc.prompt, "a prompt\n")
			_, err := config.Parse(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestSystemPromptFileSizeLimit(t *testing.T) {
	body := "scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=\"big.txt\"),\n)\n"
	_, cfg := promptTree(t, "scenario.star", body, "big.txt", strings.Repeat("x", config.MaxSystemPromptBytes+1))
	_, err := config.Parse(cfg)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v, want the size limit reported", err)
	}
}

// A scenario submitted as source or as compiled IR has no config directory to
// resolve against, so it must not be able to name a local file at all.
func TestSystemPromptFileRejectedWithoutAConfigFile(t *testing.T) {
	src := "scenario(\n    stages = [stage(\"60s\")],\n    workload = workload(\"faker\", system_prompt_file=\"preamble.txt\"),\n)\n"
	_, err := config.ParseStarlarkSource("<mcp>", src)
	if err == nil || !strings.Contains(err.Error(), "only read from a config file on disk") {
		t.Fatalf("ParseStarlarkSource error = %v, want the option refused", err)
	}

	ir := `{"workload":{"type":"faker","system_prompt_file":"preamble.txt"},"stages":[{"duration":60000000000,"concurrency":1}]}`
	if _, err := config.ParseScenarioIR(ir); err == nil || !strings.Contains(err.Error(), "only read from a config file on disk") {
		t.Fatalf("ParseScenarioIR error = %v, want the option refused", err)
	}
}

func quote(s string) string {
	return `"` + s + `"`
}
