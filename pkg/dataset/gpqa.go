package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

// GPQA generates chain-of-thought completions from GPQA multiple-choice questions.
// Uses the completions API with CoT prompting to match lm_eval's gpqa_cot_zeroshot task.
//
// Supports two data formats:
//   - Idavidrein/gpqa: separate "Correct Answer" and "Incorrect Answer 1/2/3" fields
//   - fingertap/GPQA-Diamond: choices inline in "question", answer is a bare letter in "answer"
type GPQA struct {
	items []gpqaItem
	idx   atomic.Uint64
}

type gpqaItem struct {
	// For Idavidrein format: question + separate choices
	Question string
	Choices  [4]string
	Answer   string // "(A)", "(B)", "(C)", or "(D)"

	// For fingertap format: question already has choices inline
	InlinePrompt bool
}

// NewGPQA creates a GPQA dataset from a JSONL or JSON file.
func NewGPQA(path string) (*GPQA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading gpqa file %s: %w", path, err)
	}

	items, err := parseAndBuildGPQA(data)
	if err != nil {
		return nil, fmt.Errorf("parsing gpqa file %s: %w", path, err)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("no questions found in %s", path)
	}

	return &GPQA{items: items}, nil
}

func (g *GPQA) NextConversation() Conversation {
	idx := g.idx.Add(1) - 1
	item := g.items[idx%uint64(len(g.items))]

	var prompt string
	if item.InlinePrompt {
		// fingertap format: question already contains choices
		var b strings.Builder
		fmt.Fprintf(&b, "What is the correct answer to this question: %s\n", item.Question)
		b.WriteString("Let's think step by step: ")
		prompt = b.String()
	} else {
		// Idavidrein format: build prompt with labeled choices
		var b strings.Builder
		fmt.Fprintf(&b, "What is the correct answer to this question: %s\n", item.Question)
		b.WriteString("Choices:\n")
		for i, choice := range item.Choices {
			fmt.Fprintf(&b, "(%c) %s\n", 'A'+i, choice)
		}
		b.WriteString("Let's think step by step: ")
		prompt = b.String()
	}

	greedy := 0.0
	return Conversation{
		Prompt:         prompt,
		MaxTokens:      1024,
		Stop:           []string{"</s>", "<|im_end|>"},
		Temperature:    &greedy,
		ExpectedAnswer: item.Answer,
	}
}

// gpqaRawItem supports both formats via a single struct.
type gpqaRawItem struct {
	// Idavidrein format (capitalized keys)
	QuestionCap      string `json:"Question"`
	CorrectAnswer    string `json:"Correct Answer"`
	IncorrectAnswer1 string `json:"Incorrect Answer 1"`
	IncorrectAnswer2 string `json:"Incorrect Answer 2"`
	IncorrectAnswer3 string `json:"Incorrect Answer 3"`

	// fingertap format (lowercase keys)
	QuestionLower string `json:"question"`
	AnswerLower   string `json:"answer"`
}

func parseAndBuildGPQA(data []byte) ([]gpqaItem, error) {
	rawItems, err := parseGPQARaw(data)
	if err != nil {
		return nil, err
	}

	items := make([]gpqaItem, 0, len(rawItems))
	for i, raw := range rawItems {
		if raw.CorrectAnswer != "" {
			// Idavidrein format: shuffle choices
			items = append(items, buildIdavidreinItem(raw, i))
		} else if raw.QuestionLower != "" {
			// fingertap format: choices inline
			items = append(items, buildFingertapItem(raw))
		}
	}
	return items, nil
}

func buildIdavidreinItem(raw gpqaRawItem, seed int) gpqaItem {
	choices := [4]string{
		preprocess(raw.CorrectAnswer),
		preprocess(raw.IncorrectAnswer1),
		preprocess(raw.IncorrectAnswer2),
		preprocess(raw.IncorrectAnswer3),
	}
	r := rand.New(rand.NewSource(int64(seed)))
	perm := r.Perm(4)
	var shuffled [4]string
	correctIdx := 0
	for j, p := range perm {
		shuffled[j] = choices[p]
		if p == 0 {
			correctIdx = j
		}
	}
	return gpqaItem{
		Question: preprocess(raw.QuestionCap),
		Choices:  shuffled,
		Answer:   fmt.Sprintf("(%c)", 'A'+correctIdx),
	}
}

func buildFingertapItem(raw gpqaRawItem) gpqaItem {
	letter := strings.TrimSpace(strings.ToUpper(raw.AnswerLower))
	return gpqaItem{
		Question:     raw.QuestionLower,
		Answer:       fmt.Sprintf("(%s)", letter),
		InlinePrompt: true,
	}
}

// preprocess cleans up GPQA text, matching lm_eval's preprocessing.
var bracketRe = regexp.MustCompile(`\[.*?\]`)

func preprocess(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, " [title]", ". ")
	text = bracketRe.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "  ", " ")
	return text
}

// parseGPQARaw parses GPQA data as JSONL or JSON array.
func parseGPQARaw(data []byte) ([]gpqaRawItem, error) {
	var items []gpqaRawItem
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var item gpqaRawItem
		if err := dec.Decode(&item); err != nil {
			items = nil
			break
		}
		if item.QuestionCap != "" || item.QuestionLower != "" {
			items = append(items, item)
		}
	}
	if len(items) > 0 {
		return items, nil
	}

	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}
