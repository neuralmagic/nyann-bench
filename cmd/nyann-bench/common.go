package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

// calibrateTokenRatio calibrates the chars-per-token ratio using the endpoint's tokenizer.
func calibrateTokenRatio(ctx context.Context, c *client.Client, model string, configured float64) float64 {
	if configured > 0 {
		slog.Info("Using configured chars/token", "ratio", configured)
		return configured
	}
	sample := "The quick brown fox jumps over the lazy dog. This is a sample of natural English text used to calibrate the tokenizer ratio for accurate input sequence length targeting."
	calibrated, err := c.CalibrateTokenRatio(ctx, sample, model)
	if err != nil {
		slog.Warn("Tokenizer calibration failed, using default", "error", err, "default", 4.0)
		return 4.0
	}
	slog.Info("Calibrated chars/token", "ratio", calibrated)
	return calibrated
}

// buildDataset constructs a dataset from the workload config.
func buildDataset(w *config.Workload, charsPerToken float64) (dataset.Dataset, error) {
	subISL := 0
	if w.SubsequentISL != nil {
		subISL = *w.SubsequentISL
	}

	switch w.Type {
	case "synthetic":
		ds := dataset.NewSynthetic(w.ISL, w.OSL, w.Turns, charsPerToken)
		ds.SubsequentISL = subISL
		return ds, nil
	case "faker":
		ds := dataset.NewFaker(w.ISL, w.OSL, w.Turns, charsPerToken)
		ds.SubsequentISL = subISL
		return ds, nil
	case "corpus":
		if w.CorpusPath == "" {
			return nil, fmt.Errorf("workload.corpus_path is required for corpus type")
		}
		ds, err := dataset.NewCorpus(w.CorpusPath, w.ISL, w.OSL, w.Turns, charsPerToken)
		if err != nil {
			return nil, err
		}
		ds.SubsequentISL = subISL
		return ds, nil
	case "gsm8k":
		if w.GSM8KPath == "" {
			return nil, fmt.Errorf("workload.gsm8k_path is required for gsm8k type")
		}
		w.OSL = 0
		numFewShot := 5
		if w.NumFewShot != nil {
			numFewShot = *w.NumFewShot
		}
		if numFewShot > 0 && w.GSM8KTrainPath == "" {
			return nil, fmt.Errorf("workload.gsm8k_train_path is required when num_fewshot > 0")
		}
		return dataset.NewGSM8K(w.GSM8KPath, w.GSM8KTrainPath, numFewShot)
	default:
		return nil, fmt.Errorf("unknown workload type: %s (options: synthetic, faker, corpus, gsm8k)", w.Type)
	}
}
