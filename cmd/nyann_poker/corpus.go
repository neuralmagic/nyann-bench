package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func corpusCmd() *cobra.Command {
	var (
		input  string
		output string
		format string
	)

	cmd := &cobra.Command{
		Use:   "corpus",
		Short: "Extract text from various sources into a flat corpus file",
		Long: `Extract text from various sources into a flat corpus file
for use with the corpus dataset type.

Formats:
  sharegpt    ShareGPT JSON (array of conversations)
  text        Plain text file (copy as-is)
  dir         Directory of source files (recursive)
  auto        Auto-detect from input path (default)

Examples:
  nyann_poker corpus --input ShareGPT_V3.json --output sharegpt.txt
  nyann_poker corpus --input ./vllm/ --output code.txt
  nyann_poker corpus --input novel.txt --output prose.txt

Combine multiple corpora:
  cat sharegpt.txt code.txt > mixed.txt`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return fmt.Errorf("--input is required")
			}
			if output == "" {
				return fmt.Errorf("--output is required")
			}

			if format == "auto" {
				format = detectFormat(input)
			}

			var text string
			var err error

			switch format {
			case "sharegpt":
				text, err = extractShareGPT(input)
			case "text":
				text, err = extractText(input)
			case "dir":
				text, err = extractDir(input)
			default:
				return fmt.Errorf("unknown format: %s (options: sharegpt, text, dir, auto)", format)
			}
			if err != nil {
				return err
			}

			if err := os.WriteFile(output, []byte(text), 0o644); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}

			slog.Info("Wrote corpus", "chars", len(text), "tokens_approx", len(text)/4, "path", output)
			return nil
		},
	}

	cmd.Flags().StringVar(&input, "input", "", "Input file or directory")
	cmd.Flags().StringVar(&output, "output", "", "Output corpus text file")
	cmd.Flags().StringVar(&format, "format", "auto", "Input format (sharegpt, text, dir, auto)")

	return cmd
}

func detectFormat(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "text"
	}
	if info.IsDir() {
		return "dir"
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" || ext == ".jsonl" {
		return "sharegpt"
	}
	return "text"
}

// extractShareGPT reads a ShareGPT JSON file and concatenates all conversation text.
func extractShareGPT(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	var entries []struct {
		Conversations []struct {
			From  string `json:"from"`
			Value string `json:"value"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return "", fmt.Errorf("parsing ShareGPT JSON: %w", err)
	}

	var b strings.Builder
	for _, entry := range entries {
		for _, turn := range entry.Conversations {
			b.WriteString(turn.Value)
			b.WriteByte('\n')
		}
	}

	if b.Len() == 0 {
		return "", fmt.Errorf("no conversation text found in %s", path)
	}
	return b.String(), nil
}

func extractText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func extractDir(path string) (string, error) {
	textExts := map[string]bool{
		".txt": true, ".md": true, ".py": true, ".go": true,
		".js": true, ".ts": true, ".java": true, ".c": true,
		".h": true, ".cpp": true, ".rs": true, ".rb": true,
		".sh": true, ".yaml": true, ".yml": true, ".json": true,
		".toml": true, ".cfg": true, ".ini": true, ".xml": true,
		".html": true, ".css": true, ".sql": true, ".r": true,
		".scala": true, ".kt": true, ".swift": true, ".ex": true,
		".erl": true, ".hs": true, ".ml": true, ".lisp": true,
		".el": true, ".vim": true, ".lua": true, ".pl": true,
		".pm": true, ".tex": true, ".rst": true, ".org": true,
	}

	var b strings.Builder
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !textExts[ext] {
			return nil
		}
		if info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		b.WriteString(string(data))
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no text files found in %s", path)
	}
	return b.String(), nil
}
