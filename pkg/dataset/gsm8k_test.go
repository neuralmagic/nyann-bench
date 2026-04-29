package dataset_test

import (
	"fmt"
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

func TestGSM8KPartitionDisjoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	// 7 items across 3 workers: workers get 3, 2, 2 items
	var items []string
	for i := 1; i <= 7; i++ {
		items = append(items, fmt.Sprintf(`{"question":"Q%d","answer":"#### %d"}`, i, i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(items, "\n")), 0644); err != nil {
		t.Fatal(err)
	}

	numWorkers := 3
	allAnswers := map[string]int{} // answer -> worker that got it

	for w := 0; w < numWorkers; w++ {
		ds, err := dataset.NewGSM8K(path, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		ds.Partition(w, numWorkers)

		if w < 7%numWorkers {
			if ds.Len() != 7/numWorkers+1 {
				t.Errorf("worker %d: expected %d items, got %d", w, 7/numWorkers+1, ds.Len())
			}
		} else {
			if ds.Len() != 7/numWorkers {
				t.Errorf("worker %d: expected %d items, got %d", w, 7/numWorkers, ds.Len())
			}
		}

		for i := 0; i < ds.Len(); i++ {
			conv := ds.NextConversation()
			if prev, ok := allAnswers[conv.ExpectedAnswer]; ok {
				t.Errorf("item with answer %q assigned to both worker %d and worker %d", conv.ExpectedAnswer, prev, w)
			}
			allAnswers[conv.ExpectedAnswer] = w
		}
	}

	// All 7 items should be covered
	if len(allAnswers) != 7 {
		t.Errorf("expected 7 unique items across all workers, got %d", len(allAnswers))
	}
}

func TestGSM8KPartitionEvenDivisibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	// 6 items across 3 workers: each gets exactly 2
	var items []string
	for i := 1; i <= 6; i++ {
		items = append(items, fmt.Sprintf(`{"question":"Q%d","answer":"#### %d"}`, i, i))
	}
	if err := os.WriteFile(path, []byte(strings.Join(items, "\n")), 0644); err != nil {
		t.Fatal(err)
	}

	for w := 0; w < 3; w++ {
		ds, err := dataset.NewGSM8K(path, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		ds.Partition(w, 3)
		if ds.Len() != 2 {
			t.Errorf("worker %d: expected 2 items, got %d", w, ds.Len())
		}
	}
}

func TestGSM8KPartitionSingleWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	data := `{"question":"Q1","answer":"#### 1"}
{"question":"Q2","answer":"#### 2"}
{"question":"Q3","answer":"#### 3"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewGSM8K(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	ds.Partition(0, 1)

	if ds.Len() != 3 {
		t.Errorf("single worker should get all 3 items, got %d", ds.Len())
	}
}

func TestPartitionSize(t *testing.T) {
	// 1319 items across 4 workers: 330, 330, 330, 329
	if got := dataset.PartitionSize(1319, 0, 4); got != 330 {
		t.Errorf("worker 0: expected 330, got %d", got)
	}
	if got := dataset.PartitionSize(1319, 1, 4); got != 330 {
		t.Errorf("worker 1: expected 330, got %d", got)
	}
	if got := dataset.PartitionSize(1319, 2, 4); got != 330 {
		t.Errorf("worker 2: expected 330, got %d", got)
	}
	if got := dataset.PartitionSize(1319, 3, 4); got != 329 {
		t.Errorf("worker 3: expected 329, got %d", got)
	}

	// Verify they sum to total
	total := 0
	for w := 0; w < 4; w++ {
		total += dataset.PartitionSize(1319, w, 4)
	}
	if total != 1319 {
		t.Errorf("partition sizes should sum to 1319, got %d", total)
	}

	// Even split
	if got := dataset.PartitionSize(100, 0, 4); got != 25 {
		t.Errorf("expected 25, got %d", got)
	}

	// Single worker gets everything
	if got := dataset.PartitionSize(1319, 0, 1); got != 1319 {
		t.Errorf("expected 1319, got %d", got)
	}
}

func TestCountGSM8KItems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gsm8k.jsonl")

	data := `{"question":"Q1","answer":"#### 1"}
{"question":"Q2","answer":"#### 2"}
{"question":"Q3","answer":"#### 3"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	count, err := dataset.CountGSM8KItems(path)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
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
