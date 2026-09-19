package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxSystemPromptBytes caps a system prompt read from disk.
const MaxSystemPromptBytes = 1 << 20

// resolveSystemPromptFiles moves each workload's system_prompt_file contents
// into SystemPrompt. Paths resolve against baseDir and may not leave it.
func (sc *ScenarioConfig) resolveSystemPromptFiles(baseDir string) error {
	if err := resolveWorkloadSystemPrompt(&sc.Workload, baseDir); err != nil {
		return fmt.Errorf("workload: %w", err)
	}
	for i := range sc.Stages {
		w := sc.Stages[i].Workload
		if w == nil {
			continue
		}
		if err := resolveWorkloadSystemPrompt(w, baseDir); err != nil {
			return fmt.Errorf("stage %d: workload: %w", i, err)
		}
	}
	return nil
}

func resolveWorkloadSystemPrompt(w *Workload, baseDir string) error {
	if w.SystemPromptFile == "" {
		return nil
	}
	if w.SystemPrompt != "" {
		return fmt.Errorf("system_prompt and system_prompt_file are mutually exclusive")
	}
	prompt, err := readContainedFile(baseDir, w.SystemPromptFile)
	if err != nil {
		return fmt.Errorf("system_prompt_file: %w", err)
	}
	w.SystemPrompt = prompt
	w.SystemPromptFile = ""
	return nil
}

// readContainedFile reads rel resolved against baseDir, rejecting absolute
// paths and anything that resolves outside baseDir, symlinks included.
func readContainedFile(baseDir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s must be a relative path", rel)
	}
	if baseDir == "" {
		baseDir = "."
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}

	target, err := filepath.Abs(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	if err := checkContained(base, target); err != nil {
		return "", err
	}
	// A symlink inside baseDir can still point out of it.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		if err := checkContained(base, resolved); err != nil {
			return "", err
		}
		target = resolved
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > MaxSystemPromptBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte limit", rel, info.Size(), MaxSystemPromptBytes)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	if len(data) > MaxSystemPromptBytes {
		return "", fmt.Errorf("%s is over the %d byte limit", rel, MaxSystemPromptBytes)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

func checkContained(base, target string) error {
	if target == base {
		return fmt.Errorf("%s is a directory", target)
	}
	if !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", target, base)
	}
	return nil
}

// validateWorkloadSystemPrompt runs after resolution, so a path that survived
// it came from an input that cannot read local files.
func validateWorkloadSystemPrompt(w *Workload) error {
	if w.SystemPromptFile == "" {
		return nil
	}
	if w.SystemPrompt != "" {
		return fmt.Errorf("system_prompt and system_prompt_file are mutually exclusive")
	}
	return fmt.Errorf("system_prompt_file is only read from a config file on disk; inline the prompt as system_prompt instead")
}
