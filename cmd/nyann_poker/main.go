package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "nyann_poker",
		Short: "High-performance LLM inference benchmarking",
	}

	root.AddCommand(generateCmd())
	root.AddCommand(mockServerCmd())
	root.AddCommand(analyzeCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
