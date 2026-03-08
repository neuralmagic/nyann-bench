package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"

	"github.com/neuralmagic/nyann_poker/pkg/eval"
)

// GSM8K generates single-turn completions from GSM8K math problems.
// Uses the completions API (not chat) to match lm_eval's gsm8k task.
// MaxTokens is deliberately not set — the model must generate freely.
type GSM8K struct {
	items    []gsm8kItem
	fewShot  []gsm8kItem // training examples for few-shot prompting
	nShot    int
	idx      atomic.Uint64
}

type gsm8kItem struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// NewGSM8K creates a GSM8K dataset.
// testPath points to the test JSONL, trainPath to the training JSONL (for few-shot).
// If numFewShot > 0, trainPath is required.
func NewGSM8K(testPath string, trainPath string, numFewShot int) (*GSM8K, error) {
	data, err := os.ReadFile(testPath)
	if err != nil {
		return nil, fmt.Errorf("reading gsm8k test file %s: %w", testPath, err)
	}

	items, err := parseGSM8K(data)
	if err != nil {
		return nil, fmt.Errorf("parsing gsm8k test file %s: %w", testPath, err)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("no questions found in %s", testPath)
	}

	var fewShot []gsm8kItem
	if numFewShot > 0 {
		if trainPath == "" {
			return nil, fmt.Errorf("gsm8k_train_path is required when num_fewshot > 0")
		}
		trainData, err := os.ReadFile(trainPath)
		if err != nil {
			return nil, fmt.Errorf("reading gsm8k train file %s: %w", trainPath, err)
		}
		fewShot, err = parseGSM8K(trainData)
		if err != nil {
			return nil, fmt.Errorf("parsing gsm8k train file %s: %w", trainPath, err)
		}
		if len(fewShot) < numFewShot {
			return nil, fmt.Errorf("need %d few-shot examples but train file has only %d", numFewShot, len(fewShot))
		}
	}

	return &GSM8K{items: items, fewShot: fewShot, nShot: numFewShot}, nil
}

func (g *GSM8K) NextConversation() Conversation {
	idx := g.idx.Add(1) - 1
	item := g.items[idx%uint64(len(g.items))]

	prompt := g.buildPrompt(item)

	greedy := 0.0
	return Conversation{
		Prompt:         prompt,
		MaxTokens:      256, // matches lm_eval default; stop sequences end it early
		Stop:           []string{"Question:", "</s>", "<|im_end|>"},
		Temperature:    &greedy,
		ExpectedAnswer: eval.ExtractExpected(item.Answer),
	}
}

// buildPrompt constructs a completions-style prompt matching lm_eval's gsm8k format:
//
//	Question: <q>
//	Answer: <a>
//
//	Question: <q>
//	Answer: <a>
//	...
//	Question: <test question>
//	Answer:
func (g *GSM8K) buildPrompt(testItem gsm8kItem) string {
	var b strings.Builder

	// Add few-shot examples from training set
	if g.nShot > 0 {
		indices := rand.Perm(len(g.fewShot))[:g.nShot]
		for i, idx := range indices {
			if i > 0 {
				b.WriteString("\n\n")
			}
			ex := g.fewShot[idx]
			fmt.Fprintf(&b, "Question: %s\nAnswer: %s", ex.Question, ex.Answer)
		}
		b.WriteString("\n\n")
	}

	// Add the test question
	fmt.Fprintf(&b, "Question: %s\nAnswer:", testItem.Question)

	return b.String()
}

// parseGSM8K parses GSM8K data as JSONL (one object per line) or JSON array.
func parseGSM8K(data []byte) ([]gsm8kItem, error) {
	// Try JSONL first
	var items []gsm8kItem
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var item gsm8kItem
		if err := dec.Decode(&item); err != nil {
			// Not valid JSONL, try array
			items = nil
			break
		}
		if item.Question != "" {
			items = append(items, item)
		}
	}
	if len(items) > 0 {
		return items, nil
	}

	// Try JSON array
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}
