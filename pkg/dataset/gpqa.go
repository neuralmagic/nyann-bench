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

	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// GPQA generates chain-of-thought completions from GPQA multiple-choice questions.
// Uses the completions API with CoT prompting to match lm_eval's gpqa_cot_zeroshot task.
//
// Supports two data formats:
//   - Idavidrein/gpqa: separate "Correct Answer" and "Incorrect Answer 1/2/3" fields
//   - fingertap/GPQA-Diamond: choices inline in question, answer is a bare letter in "answer"
type GPQA struct {
	items     []gpqaItem
	idx       atomic.Uint64
	maxTokens int
}

type gpqaItem struct {
	Question string
	Choices  [4]string
	Answer   string // "(A)", "(B)", "(C)", or "(D)"

	InlinePrompt bool // fingertap format: question already has choices inline
}

// NewGPQA creates a GPQA dataset from a JSONL or JSON file.
// maxTokens controls the max output tokens per request (0 = default 16384).
func NewGPQA(path string, maxTokens int) (*GPQA, error) {
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

	// Deterministic shuffle so all workers agree on item ordering for partitioning
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })

	if maxTokens <= 0 {
		maxTokens = 16384
	}
	return &GPQA{items: items, maxTokens: maxTokens}, nil
}

func (g *GPQA) Len() int {
	return len(g.items)
}

// Partition slices the dataset to only the items assigned to the given worker.
func (g *GPQA) Partition(workerID, numWorkers int) {
	if numWorkers <= 0 {
		panic("Partition: numWorkers must be > 0")
	}
	if workerID < 0 || workerID >= numWorkers {
		panic(fmt.Sprintf("Partition: workerID %d out of range [0, %d)", workerID, numWorkers))
	}

	n := len(g.items)
	base := n / numWorkers
	remainder := n % numWorkers

	start := workerID*base + min(workerID, remainder)
	size := base
	if workerID < remainder {
		size++
	}

	g.items = g.items[start : start+size]
	g.idx.Store(0)
}

func (g *GPQA) NextConversation() Conversation {
	idx := g.idx.Add(1) - 1
	item := g.items[idx%uint64(len(g.items))]

	var b strings.Builder
	if item.InlinePrompt {
		b.WriteString(item.Question)
	} else {
		b.WriteString(item.Question)
		b.WriteString("\n")
		for i, choice := range item.Choices {
			fmt.Fprintf(&b, "(%c) %s\n", 'A'+i, choice)
		}
	}
	b.WriteString("Express your final answer as the corresponding option 'A', 'B', 'C', or 'D'.")
	prompt := b.String()

	greedy := 0.0
	return Conversation{
		Turns: [][]client.Message{
			{{Role: "user", Content: prompt}},
		},
		MaxTokens:      g.maxTokens,
		Temperature:    &greedy,
		ExpectedAnswer: item.Answer,
	}
}

type gpqaRawItem struct {
	// Idavidrein format
	QuestionCap      string `json:"Question"`
	CorrectAnswer    string `json:"Correct Answer"`
	IncorrectAnswer1 string `json:"Incorrect Answer 1"`
	IncorrectAnswer2 string `json:"Incorrect Answer 2"`
	IncorrectAnswer3 string `json:"Incorrect Answer 3"`

	// fingertap format
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
			items = append(items, buildIdavidreinItem(raw, i))
		} else if raw.QuestionLower != "" {
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

var bracketRe = regexp.MustCompile(`\[.*?\]`)

func preprocess(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, " [title]", ". ")
	text = bracketRe.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "  ", " ")
	return text
}

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
