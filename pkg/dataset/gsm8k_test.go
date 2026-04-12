package dataset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

func TestGSM8KFromJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	data := `{"question":"If there are 3 cars and each has 4 wheels, how many wheels total?","answer":"3 cars * 4 wheels = 12 wheels\n#### 12"}
{"question":"A baker made 24 cookies and ate 3. How many are left?","answer":"24 - 3 = 21\n#### 21"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if conv.Prompt == "" {
		t.Fatal("expected non-empty Prompt for completions API")
	}
	if len(conv.Turns) != 0 {
		t.Errorf("expected no Turns for completions mode, got %d", len(conv.Turns))
	}
	if conv.MaxTokens != 2048 {
		t.Errorf("expected MaxTokens=2048, got %d", conv.MaxTokens)
	}
	if conv.ExpectedAnswer != "12" && conv.ExpectedAnswer != "21" {
		t.Errorf("expected answer '12' or '21', got %q", conv.ExpectedAnswer)
	}
	// 0-shot: should just be "Question: ...\nAnswer:"
	if !strings.HasPrefix(conv.Prompt, "Question: ") {
		t.Errorf("unexpected prompt start: %s", conv.Prompt[:50])
	}
	if !strings.HasSuffix(conv.Prompt, "\nAnswer:") {
		t.Errorf("expected prompt to end with 'Answer:', got: ...%s", conv.Prompt[len(conv.Prompt)-20:])
	}
}

func TestGSM8KFromJSONArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.json")

	data := `[{"question":"What is 2+2?","answer":"#### 4"},{"question":"What is 3*5?","answer":"#### 15"}]`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	c1 := ds.NextConversation()
	c2 := ds.NextConversation()

	// Order is randomized, but both answers should be present
	answers := map[string]bool{c1.ExpectedAnswer: true, c2.ExpectedAnswer: true}
	if !answers["4"] || !answers["15"] {
		t.Errorf("expected answers '4' and '15', got %q and %q", c1.ExpectedAnswer, c2.ExpectedAnswer)
	}
}

func TestGSM8KWrapsAround(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	data := `{"question":"Q1","answer":"#### 1"}
{"question":"Q2","answer":"#### 2"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Consume both items, then should wrap around to a valid answer
	ds.NextConversation()
	ds.NextConversation()
	c3 := ds.NextConversation()

	if c3.ExpectedAnswer != "1" && c3.ExpectedAnswer != "2" {
		t.Errorf("expected wrap-around to '1' or '2', got %q", c3.ExpectedAnswer)
	}
}

func TestGSM8KEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := dataset.NewGSM8K(path, "", 0)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestGSM8KFewShot(t *testing.T) {
	dir := t.TempDir()
	testPath := filepath.Join(dir, "test.jsonl")
	trainPath := filepath.Join(dir, "train.jsonl")

	testData := `{"question":"What is 10+5?","answer":"10 + 5 = 15\n#### 15"}`
	trainData := `{"question":"What is 1+1?","answer":"1 + 1 = 2\n#### 2"}
{"question":"What is 2+2?","answer":"2 + 2 = 4\n#### 4"}
{"question":"What is 3+3?","answer":"3 + 3 = 6\n#### 6"}
`
	os.WriteFile(testPath, []byte(testData), 0644)
	os.WriteFile(trainPath, []byte(trainData), 0644)

	ds, err := dataset.NewGSM8K(testPath, trainPath, 2)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if conv.Prompt == "" {
		t.Fatal("expected non-empty Prompt")
	}

	// Should contain few-shot examples before the test question
	if !strings.Contains(conv.Prompt, "Question: What is 10+5?") {
		t.Error("prompt should contain test question")
	}
	if !strings.HasSuffix(conv.Prompt, "\nAnswer:") {
		t.Errorf("prompt should end with 'Answer:', got: ...%s", conv.Prompt[len(conv.Prompt)-20:])
	}

	// Count "Question:" occurrences — should be 3 (2 few-shot + 1 test)
	count := strings.Count(conv.Prompt, "Question:")
	if count != 3 {
		t.Errorf("expected 3 'Question:' occurrences (2 few-shot + 1 test), got %d", count)
	}

	// Few-shot answers should appear in the prompt
	if !strings.Contains(conv.Prompt, "Answer:") {
		t.Error("prompt should contain 'Answer:' from few-shot examples")
	}

	if conv.ExpectedAnswer != "15" {
		t.Errorf("expected answer '15', got %q", conv.ExpectedAnswer)
	}
}

func TestGSM8KFewShotRequiresTrainPath(t *testing.T) {
	dir := t.TempDir()
	testPath := filepath.Join(dir, "test.jsonl")
	os.WriteFile(testPath, []byte(`{"question":"Q","answer":"#### 1"}`), 0644)

	_, err := dataset.NewGSM8K(testPath, "", 5)
	if err == nil {
		t.Error("expected error when num_fewshot > 0 without train path")
	}
}
