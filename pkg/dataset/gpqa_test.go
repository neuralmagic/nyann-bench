package dataset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann_poker/pkg/dataset"
)

func TestGPQAFromJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	data := `{"Question":"What is the primary function of mitochondria?","Correct Answer":"ATP production via oxidative phosphorylation","Incorrect Answer 1":"Protein synthesis","Incorrect Answer 2":"DNA replication","Incorrect Answer 3":"Cell division"}
{"Question":"What is Planck's constant?","Correct Answer":"6.626e-34 J·s","Incorrect Answer 1":"3.0e8 m/s","Incorrect Answer 2":"1.38e-23 J/K","Incorrect Answer 3":"9.81 m/s²"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGPQA(path)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if conv.Prompt == "" {
		t.Fatal("expected non-empty Prompt")
	}
	if !strings.Contains(conv.Prompt, "What is the primary function of mitochondria?") {
		t.Error("prompt should contain the question")
	}
	if !strings.Contains(conv.Prompt, "(A)") || !strings.Contains(conv.Prompt, "(D)") {
		t.Error("prompt should contain choice labels (A) through (D)")
	}
	if !strings.Contains(conv.Prompt, "Let's think step by step:") {
		t.Error("prompt should end with CoT instruction")
	}
	if conv.MaxTokens != 1024 {
		t.Errorf("expected MaxTokens=1024, got %d", conv.MaxTokens)
	}
	// Answer should be one of (A)-(D)
	if conv.ExpectedAnswer != "(A)" && conv.ExpectedAnswer != "(B)" &&
		conv.ExpectedAnswer != "(C)" && conv.ExpectedAnswer != "(D)" {
		t.Errorf("expected answer (A)-(D), got %q", conv.ExpectedAnswer)
	}
	// The correct answer text should appear somewhere in the prompt
	if !strings.Contains(conv.Prompt, "ATP production via oxidative phosphorylation") {
		t.Error("prompt should contain the correct answer text")
	}
	if conv.Temperature == nil || *conv.Temperature != 0.0 {
		t.Error("expected temperature=0.0")
	}
}

func TestGPQAShufflesChoices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	// With shuffling, the correct answer shouldn't always be at the same position
	data := `{"Question":"Q1","Correct Answer":"Correct1","Incorrect Answer 1":"Wrong1a","Incorrect Answer 2":"Wrong1b","Incorrect Answer 3":"Wrong1c"}
{"Question":"Q2","Correct Answer":"Correct2","Incorrect Answer 1":"Wrong2a","Incorrect Answer 2":"Wrong2b","Incorrect Answer 3":"Wrong2c"}
{"Question":"Q3","Correct Answer":"Correct3","Incorrect Answer 1":"Wrong3a","Incorrect Answer 2":"Wrong3b","Incorrect Answer 3":"Wrong3c"}
{"Question":"Q4","Correct Answer":"Correct4","Incorrect Answer 1":"Wrong4a","Incorrect Answer 2":"Wrong4b","Incorrect Answer 3":"Wrong4c"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGPQA(path)
	if err != nil {
		t.Fatal(err)
	}

	answers := map[string]bool{}
	for i := 0; i < 4; i++ {
		conv := ds.NextConversation()
		answers[conv.ExpectedAnswer] = true
	}
	// With 4 items and per-item seeded shuffling, we should get at least 2 different answer positions
	if len(answers) < 2 {
		t.Errorf("expected shuffled answer positions, got %v", answers)
	}
}

func TestGPQAWrapsAround(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	data := `{"Question":"Q1","Correct Answer":"C1","Incorrect Answer 1":"I1","Incorrect Answer 2":"I2","Incorrect Answer 3":"I3"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGPQA(path)
	if err != nil {
		t.Fatal(err)
	}

	c1 := ds.NextConversation()
	c2 := ds.NextConversation()
	if c1.ExpectedAnswer != c2.ExpectedAnswer {
		t.Error("single-item dataset should wrap around to same answer")
	}
}

func TestGPQAEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := dataset.NewGPQA(path)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestGPQAFingertapFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	// fingertap/GPQA-Diamond format: lowercase keys, choices inline in question, bare letter answer
	data := `{"question":"Among the following, which has the highest density? a) Earth-mass planet b) 2x Earth mass c) 5x Earth mass d) Half Earth mass","answer":"D"}
{"question":"What is Planck's constant? A. 6.626e-34 B. 3.0e8 C. 1.38e-23 D. 9.81","answer":"A"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGPQA(path)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if conv.Prompt == "" {
		t.Fatal("expected non-empty Prompt")
	}
	if !strings.Contains(conv.Prompt, "Among the following") {
		t.Error("prompt should contain the question")
	}
	// fingertap format: choices are already inline, so no separate (A)-(D) labels added
	if !strings.Contains(conv.Prompt, "Let's think step by step:") {
		t.Error("prompt should contain CoT instruction")
	}
	if conv.ExpectedAnswer != "(D)" {
		t.Errorf("expected answer (D), got %q", conv.ExpectedAnswer)
	}

	conv2 := ds.NextConversation()
	if conv2.ExpectedAnswer != "(A)" {
		t.Errorf("expected answer (A), got %q", conv2.ExpectedAnswer)
	}
}

func TestGPQAPreprocess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	// Include text with [title] and [bracket annotations] that should be cleaned
	data := `{"Question":"In physics [title] what is force?","Correct Answer":"Mass times acceleration [Newton]","Incorrect Answer 1":"Speed [ref]","Incorrect Answer 2":"Energy","Incorrect Answer 3":"Power"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGPQA(path)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if strings.Contains(conv.Prompt, "[title]") {
		t.Error("prompt should not contain [title]")
	}
	if strings.Contains(conv.Prompt, "[Newton]") {
		t.Error("prompt should not contain bracket annotations")
	}
	if !strings.Contains(conv.Prompt, "In physics.") {
		t.Error("expected [title] replaced with period")
	}
}
