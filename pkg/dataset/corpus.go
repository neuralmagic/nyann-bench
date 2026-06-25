package dataset

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// Corpus generates conversations by sliding a window over real text files.
type Corpus struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64

	text       string // concatenated corpus text
	mu         sync.RWMutex
	baseOffset uint64
	offset     atomic.Uint64
}

func NewCorpus(corpusPath string, isl, osl, turns int, charsPerToken float64) (*Corpus, error) {
	if turns < 1 {
		turns = 1
	}

	text, err := loadCorpusText(corpusPath)
	if err != nil {
		return nil, fmt.Errorf("loading corpus from %s: %w", corpusPath, err)
	}

	if len(text) == 0 {
		return nil, fmt.Errorf("corpus at %s is empty", corpusPath)
	}

	c := &Corpus{ISL: isl, OSL: osl, Turns: turns, CharsPerToken: charsPerToken, text: text}
	// Start at a random offset so multiple workers don't read the same text
	c.setBaseOffset(uint64(rand.Intn(len(text))))
	return c, nil
}

// SetOffsetSeed makes the starting corpus window deterministic.
func (c *Corpus) SetOffsetSeed(seed int64) {
	rng := rand.New(rand.NewSource(seed))
	c.setBaseOffset(uint64(rng.Intn(len(c.text))))
}

func (c *Corpus) setBaseOffset(offset uint64) {
	c.baseOffset = offset
	c.offset.Store(offset)
}

func (c *Corpus) turnISL(t int) int {
	if t > 0 && c.SubsequentISL > 0 {
		return c.SubsequentISL
	}
	return c.ISL
}

func (c *Corpus) NextConversation() Conversation {
	start := c.offset.Add(uint64(c.conversationChars())) - uint64(c.conversationChars())
	return c.conversationFromOffset(start)
}

func (c *Corpus) ConversationAt(index int) Conversation {
	if index < 0 {
		index = 0
	}
	start := c.baseOffset + uint64(index*c.conversationChars())
	return c.conversationFromOffset(start)
}

func (c *Corpus) conversationFromOffset(start uint64) Conversation {
	turns := make([][]client.Message, c.Turns)
	var history []client.Message
	cursor := start

	for t := 0; t < c.Turns; t++ {
		chunk := c.chunkAt(cursor, c.turnISL(t))
		cursor += uint64(c.targetChars(c.turnISL(t)))
		userMsg := client.Message{
			Role:    "user",
			Content: chunk,
		}
		history = append(history, userMsg)

		turnMsgs := make([]client.Message, len(history))
		copy(turnMsgs, history)
		turns[t] = turnMsgs

		if t < c.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: c.chunkAt(cursor, c.OSL),
			})
			cursor += uint64(c.targetChars(c.OSL))
		}
	}

	return Conversation{Turns: turns, MaxTokens: c.OSL}
}

func (c *Corpus) conversationChars() int {
	total := 0
	for t := 0; t < c.Turns; t++ {
		total += c.targetChars(c.turnISL(t))
		if t < c.Turns-1 {
			total += c.targetChars(c.OSL)
		}
	}
	if total < 1 {
		return 1
	}
	return total
}

func (c *Corpus) targetChars(targetTokens int) int {
	targetChars := int(float64(targetTokens) * c.CharsPerToken)
	if targetChars < 0 {
		return 0
	}
	return targetChars
}

// nextChunk returns approximately targetTokens worth of text from the corpus,
// advancing the shared offset. Wraps around when reaching the end.
func (c *Corpus) nextChunk(targetTokens int) string {
	targetChars := c.targetChars(targetTokens)
	textLen := uint64(len(c.text))

	// Atomically claim a range of the corpus
	start := c.offset.Add(uint64(targetChars)) - uint64(targetChars)
	start = start % textLen

	end := start + uint64(targetChars)
	if end <= textLen {
		return c.text[start:end]
	}

	// Wrap around
	var b strings.Builder
	b.WriteString(c.text[start:])
	remaining := targetChars - (int(textLen) - int(start))
	wrapped := uint64(remaining) % textLen
	b.WriteString(c.text[:wrapped])
	return b.String()
}

func (c *Corpus) chunkAt(start uint64, targetTokens int) string {
	targetChars := c.targetChars(targetTokens)
	textLen := uint64(len(c.text))
	start = start % textLen

	end := start + uint64(targetChars)
	if end <= textLen {
		return c.text[start:end]
	}

	var b strings.Builder
	b.WriteString(c.text[start:])
	remaining := targetChars - (int(textLen) - int(start))
	wrapped := uint64(remaining) % textLen
	b.WriteString(c.text[:wrapped])
	return b.String()
}

// loadCorpusText reads all text files from a path (file or directory).
func loadCorpusText(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	// Directory: concatenate all text-like files
	var b strings.Builder
	textExts := map[string]bool{
		".txt": true, ".md": true, ".py": true, ".go": true,
		".js": true, ".ts": true, ".java": true, ".c": true,
		".h": true, ".cpp": true, ".rs": true, ".rb": true,
		".sh": true, ".yaml": true, ".yml": true, ".json": true,
		".toml": true, ".cfg": true, ".ini": true, ".xml": true,
		".html": true, ".css": true, ".sql": true, ".r": true,
		".scala": true, ".kt": true, ".swift": true, ".ex": true,
		".erl": true, ".hs": true, ".ml": true, ".lisp": true,
		".el": true, ".vim": true, ".lua": true, ".pl": true,
		".pm": true, ".tex": true, ".rst": true, ".org": true,
	}

	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			// Skip hidden and vendor directories
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !textExts[ext] {
			return nil
		}
		// Skip large files (>1MB)
		if info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		b.WriteString(string(data))
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		return "", err
	}

	return b.String(), nil
}
