package dataset_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

func gpqaPrompt(conv dataset.Conversation) string {
	if len(conv.Turns) == 0 || len(conv.Turns[0]) == 0 {
		return ""
	}
	return conv.Turns[0][0].Content
}

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

	if ds.Len() != 2 {
		t.Fatalf("expected 2 items, got %d", ds.Len())
	}

	conv := ds.NextConversation()
	prompt := gpqaPrompt(conv)
	if prompt == "" {
		t.Fatal("expected non-empty prompt in Turns")
	}
	if conv.Prompt != "" {
		t.Error("should use chat API (Turns), not completions API (Prompt)")
	}
	if !strings.Contains(prompt, "(A)") || !strings.Contains(prompt, "(D)") {
		t.Error("prompt should contain choice labels (A) through (D)")
	}
	if !strings.Contains(prompt, "Express your final answer") {
		t.Error("prompt should contain answer instruction")
	}
	if conv.MaxTokens != 1024 {
		t.Errorf("expected MaxTokens=1024, got %d", conv.MaxTokens)
	}
	if conv.ExpectedAnswer != "(A)" && conv.ExpectedAnswer != "(B)" &&
		conv.ExpectedAnswer != "(C)" && conv.ExpectedAnswer != "(D)" {
		t.Errorf("expected answer (A)-(D), got %q", conv.ExpectedAnswer)
	}
	if conv.Temperature == nil || *conv.Temperature != 0.0 {
		t.Error("expected temperature=0.0")
	}
}

func TestGPQAShufflesChoices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	var items []string
	for i := 0; i < 4; i++ {
		items = append(items, fmt.Sprintf(`{"Question":"Q%d","Correct Answer":"Correct%d","Incorrect Answer 1":"Wrong%da","Incorrect Answer 2":"Wrong%db","Incorrect Answer 3":"Wrong%dc"}`, i, i, i, i, i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(items, "\n")), 0644); err != nil {
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

	answers := map[string]bool{}
	for i := 0; i < 2; i++ {
		conv := ds.NextConversation()
		prompt := gpqaPrompt(conv)
		if prompt == "" {
			t.Fatal("expected non-empty prompt in Turns")
		}
		if !strings.Contains(prompt, "Express your final answer") {
			t.Error("prompt should contain answer instruction")
		}
		answers[conv.ExpectedAnswer] = true
	}
	if !answers["(D)"] || !answers["(A)"] {
		t.Errorf("expected answers (D) and (A), got %v", answers)
	}
}

func TestGPQAPreprocess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

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
	prompt := gpqaPrompt(conv)
	if strings.Contains(prompt, "[title]") {
		t.Error("prompt should not contain [title]")
	}
	if strings.Contains(prompt, "[Newton]") {
		t.Error("prompt should not contain bracket annotations")
	}
	if !strings.Contains(prompt, "In physics.") {
		t.Error("expected [title] replaced with period")
	}
}

func TestGPQAPartitionDisjoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpqa.jsonl")

	var items []string
	for i := 0; i < 7; i++ {
		items = append(items, fmt.Sprintf(`{"Question":"Q%d","Correct Answer":"C%d","Incorrect Answer 1":"I%da","Incorrect Answer 2":"I%db","Incorrect Answer 3":"I%dc"}`, i, i, i, i, i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(items, "\n")), 0644); err != nil {
		t.Fatal(err)
	}

	numWorkers := 3
	allPrompts := map[string]int{}

	total := 0
	for w := 0; w < numWorkers; w++ {
		ds, err := dataset.NewGPQA(path)
		if err != nil {
			t.Fatal(err)
		}
		ds.Partition(w, numWorkers)
		total += ds.Len()

		for i := 0; i < ds.Len(); i++ {
			conv := ds.NextConversation()
			prompt := gpqaPrompt(conv)
			if prev, ok := allPrompts[prompt]; ok {
				t.Errorf("item assigned to both worker %d and worker %d", prev, w)
			}
			allPrompts[prompt] = w
		}
	}

	if total != 7 {
		t.Errorf("expected 7 total items across partitions, got %d", total)
	}
	if len(allPrompts) != 7 {
		t.Errorf("expected 7 unique prompts, got %d", len(allPrompts))
	}
}
