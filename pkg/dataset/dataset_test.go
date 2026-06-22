package dataset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

func TestSyntheticBasic(t *testing.T) {
	ds := dataset.NewSynthetic(128, 256, 2, 4.0)
	conv := ds.NextConversation()

	if len(conv.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(conv.Turns))
	}
	if conv.MaxTokens != 256 {
		t.Errorf("expected MaxTokens=256, got %d", conv.MaxTokens)
	}
	// Turn 0: 1 user message
	if len(conv.Turns[0]) != 1 {
		t.Errorf("turn 0: expected 1 message, got %d", len(conv.Turns[0]))
	}
	// Turn 1: user + assistant + user = 3 messages
	if len(conv.Turns[1]) != 3 {
		t.Errorf("turn 1: expected 3 messages, got %d", len(conv.Turns[1]))
	}
}

func TestFakerBasic(t *testing.T) {
	ds := dataset.NewFaker(128, 256, 2, 4.0)
	conv := ds.NextConversation()

	if len(conv.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(conv.Turns))
	}
	if conv.MaxTokens != 256 {
		t.Errorf("expected MaxTokens=256, got %d", conv.MaxTokens)
	}

	// Check content is non-trivial
	content := conv.Turns[0][0].Content
	if len(content) < 100 {
		t.Errorf("expected substantial content, got %d chars", len(content))
	}
}

func TestFakerDeterministic(t *testing.T) {
	// Two separate Faker instances should produce different conversations
	// because they use atomic sequence numbers
	ds := dataset.NewFaker(64, 64, 1, 4.0)
	c1 := ds.NextConversation()
	c2 := ds.NextConversation()

	if c1.Turns[0][0].Content == c2.Turns[0][0].Content {
		t.Error("expected different content for sequential conversations")
	}
}

func TestCorpusFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	// Create a corpus file with enough text
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewCorpus(path, 32, 16, 1, 4.0, nil)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if len(conv.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(conv.Turns))
	}
	if conv.MaxTokens != 16 {
		t.Errorf("expected MaxTokens=16, got %d", conv.MaxTokens)
	}

	content := conv.Turns[0][0].Content
	// 32 tokens * 4 chars = 128 chars
	if len(content) != 128 {
		t.Errorf("expected 128 chars, got %d", len(content))
	}
}

func TestCorpusFromDirectory(t *testing.T) {
	dir := t.TempDir()

	// Create a few text files
	for _, name := range []string{"a.py", "b.go", "c.txt"} {
		text := strings.Repeat("def hello(): print('world') ", 100)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Create a non-text file that should be skipped
	if err := os.WriteFile(filepath.Join(dir, "image.png"), []byte("not text"), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewCorpus(dir, 32, 16, 1, 4.0, nil)
	if err != nil {
		t.Fatal(err)
	}

	conv := ds.NextConversation()
	if len(conv.Turns[0][0].Content) != 128 {
		t.Errorf("expected 128 chars, got %d", len(conv.Turns[0][0].Content))
	}
}

func TestCorpusSlidingWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	// Use non-repeating text so sliding window produces different chunks
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		sb.WriteByte(byte('a' + (i % 26)))
	}
	text := sb.String()
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}

	ds, err := dataset.NewCorpus(path, 16, 8, 1, 4.0, nil) // 16 tokens * 4 chars = 64 chars per chunk
	if err != nil {
		t.Fatal(err)
	}

	// Sequential chunks should start at different offsets
	c1 := ds.NextConversation()
	c2 := ds.NextConversation()

	content1 := c1.Turns[0][0].Content
	content2 := c2.Turns[0][0].Content

	if content1 == content2 {
		t.Error("expected different content for sequential corpus reads")
	}
}

func TestSubsequentISL(t *testing.T) {
	// Turn 0 should use ISL=200, turns 1+ should use SubsequentISL=50.
	charsPerToken := 4.0

	t.Run("synthetic", func(t *testing.T) {
		ds := dataset.NewSynthetic(200, 64, 3, charsPerToken)
		ds.SubsequentISL = 50
		conv := ds.NextConversation()

		turn0Len := len(conv.Turns[0][0].Content)
		// Turn 0 user message: 200 tokens * 4 chars = 800
		if turn0Len != 800 {
			t.Errorf("turn 0: expected 800 chars, got %d", turn0Len)
		}
		// Turn 1 new user message is the last in the slice
		turn1User := conv.Turns[1][len(conv.Turns[1])-1].Content
		// 50 tokens * 4 chars = 200
		if len(turn1User) != 200 {
			t.Errorf("turn 1 user: expected 200 chars, got %d", len(turn1User))
		}
		// Turn 2 new user message
		turn2User := conv.Turns[2][len(conv.Turns[2])-1].Content
		if len(turn2User) != 200 {
			t.Errorf("turn 2 user: expected 200 chars, got %d", len(turn2User))
		}
	})

	t.Run("faker", func(t *testing.T) {
		ds := dataset.NewFaker(200, 64, 3, charsPerToken)
		ds.SubsequentISL = 50
		conv := ds.NextConversation()

		turn0Len := len(conv.Turns[0][0].Content)
		if turn0Len != 800 {
			t.Errorf("turn 0: expected 800 chars, got %d", turn0Len)
		}
		turn1User := conv.Turns[1][len(conv.Turns[1])-1].Content
		if len(turn1User) != 200 {
			t.Errorf("turn 1 user: expected 200 chars, got %d", len(turn1User))
		}
	})

	t.Run("default_no_subsequent", func(t *testing.T) {
		// When SubsequentISL is 0, all turns use ISL.
		ds := dataset.NewSynthetic(100, 64, 2, charsPerToken)
		conv := ds.NextConversation()

		turn0Len := len(conv.Turns[0][0].Content)
		turn1User := conv.Turns[1][len(conv.Turns[1])-1].Content
		if turn0Len != 400 {
			t.Errorf("turn 0: expected 400 chars, got %d", turn0Len)
		}
		if len(turn1User) != 400 {
			t.Errorf("turn 1 user: expected 400 chars (same as ISL), got %d", len(turn1User))
		}
	})
}

func TestCorpusEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := dataset.NewCorpus(path, 32, 16, 1, 4.0, nil)
	if err == nil {
		t.Error("expected error for empty corpus")
	}
}

func TestCorpusPreTokenize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	// Create corpus with mixed token density:
	// - dense region (short words ≈ 2 chars/token)
	// - sparse region (long words ≈ 6 chars/token)
	dense := strings.Repeat("a b c d e f g h ", 500)  // ~4000 chars, ~2 chars/tok
	sparse := strings.Repeat("elephant butterfly caterpillar ", 200) // ~6000 chars, ~6 chars/tok
	text := dense + sparse
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock token counter: 1 token per whitespace-delimited word
	wordCounter := func(s string) (int, error) {
		return len(strings.Fields(s)), nil
	}

	ds, err := dataset.NewCorpus(path, 100, 50, 1, 4.0, wordCounter)
	if err != nil {
		t.Fatal(err)
	}

	// Generate several conversations and verify token counts are close to 100
	for i := 0; i < 10; i++ {
		conv := ds.NextConversation()
		content := conv.Turns[0][0].Content
		tokens := len(strings.Fields(content))
		// With pre-tokenization, should be within ~10% of target
		if tokens < 80 || tokens > 120 {
			t.Errorf("iteration %d: expected ~100 tokens, got %d (len=%d chars)", i, tokens, len(content))
		}
	}

	// Compare with char-based corpus (no token counter) — should show more variance
	dsCharBased, err := dataset.NewCorpus(path, 100, 50, 1, 4.0, nil)
	if err != nil {
		t.Fatal(err)
	}

	var charBasedTokens []int
	for i := 0; i < 10; i++ {
		conv := dsCharBased.NextConversation()
		content := conv.Turns[0][0].Content
		charBasedTokens = append(charBasedTokens, len(strings.Fields(content)))
	}

	// Char-based should produce the same character count but wildly varying token counts
	minT, maxT := charBasedTokens[0], charBasedTokens[0]
	for _, tok := range charBasedTokens {
		if tok < minT {
			minT = tok
		}
		if tok > maxT {
			maxT = tok
		}
	}
	t.Logf("Char-based token range: %d-%d (spread=%d)", minT, maxT, maxT-minT)
}
