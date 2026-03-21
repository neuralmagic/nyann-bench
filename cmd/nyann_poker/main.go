package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	var logLevel string

	root := &cobra.Command{
		Use:   "nyann_poker",
		Short: "High-performance LLM inference benchmarking",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: level,
			})))
			return nil
		},
	}

	root.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Log level (debug, info, warn, error)")

	root.AddCommand(generateCmd())
	root.AddCommand(mockServerCmd())
	root.AddCommand(analyzeCmd())
	root.AddCommand(corpusCmd())
	root.AddCommand(characterizeCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (options: debug, info, warn, error)", s)
	}
}
