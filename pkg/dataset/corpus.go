package dataset

import (
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// Corpus generates conversations by sliding a window over real text files.
// When a TokenCounter is set, each chunk is remotely tokenized and
// proportionally trimmed toward the target token count.
type Corpus struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64

	// TokenCounter counts tokens in a string via the server's /tokenize
	// endpoint. When non-nil, nextChunk overfetches text and trims toward the
	// target token count. When nil, falls back to a char-based estimate.
	TokenCounter TokenCounter

	text   string // concatenated corpus text
	offset atomic.Uint64
}

func NewCorpus(corpusPath string, isl, osl, turns int, charsPerToken float64, tokenCounter TokenCounter) (*Corpus, error) {
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

	c := &Corpus{
		ISL:           isl,
		OSL:           osl,
		Turns:         turns,
		CharsPerToken: charsPerToken,
		TokenCounter:  tokenCounter,
		text:          text,
	}
	// Start at a random offset so multiple workers don't read the same text
	c.offset.Store(uint64(rand.Intn(len(text))))
	return c, nil
}

func (c *Corpus) turnISL(t int) int {
	if t > 0 && c.SubsequentISL > 0 {
		return c.SubsequentISL
	}
	return c.ISL
}

// NextConversation builds all turns eagerly - Corpus's generation is cheap
// enough (in-memory text slicing) that deferring it isn't worth the
// indirection, unless a TokenCounter is set (see nextChunk's doc comment).
func (c *Corpus) NextConversation() Conversation {
	turns := make([]*LazyTurn, c.Turns)
	var history []client.Message

	for t := 0; t < c.Turns; t++ {
		chunk := c.nextChunk(c.turnISL(t))
		userMsg := client.Message{
			Role:    "user",
			Content: chunk,
		}
		history = append(history, userMsg)

		turnMsgs := make([]client.Message, len(history))
		copy(turnMsgs, history)
		turns[t] = Resolved(turnMsgs)

		if t < c.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: c.nextChunk(c.OSL),
			})
		}
	}

	return Conversation{Turns: turns, MaxTokens: c.OSL}
}

// nextChunk returns targetTokens worth of text from the corpus, advancing the
// shared offset. When a TokenCounter is available, the chunk is overfetched
// and proportionally trimmed toward the token target; otherwise it falls back
// to a character estimate.
func (c *Corpus) nextChunk(targetTokens int) string {
	estimatedBytes := int(float64(targetTokens) * c.CharsPerToken)

	if c.TokenCounter == nil {
		return c.fetchText(estimatedBytes)
	}

	// Overfetch 2x, then converge proportionally using remote token counts.
	text := c.fetchText(estimatedBytes * 2)

	actualTokens, err := c.TokenCounter(text)
	if err != nil {
		slog.Debug("TokenCounter failed, using char estimate", "error", err)
		if estimatedBytes <= len(text) {
			return text[:estimatedBytes]
		}
		return text
	}

	// If 2x wasn't enough (density much lower than estimated), extend
	// using the observed density.
	if actualTokens < targetTokens && actualTokens > 0 {
		bytesPerToken := float64(len(text)) / float64(actualTokens)
		extra := int(float64(targetTokens-actualTokens)*bytesPerToken) * 2
		text += c.fetchText(extra)
		actualTokens, err = c.TokenCounter(text)
		if err != nil || actualTokens <= targetTokens {
			return text
		}
	} else if actualTokens <= targetTokens {
		return text
	}

	text = trimToTokenBudgetWithCount(text, targetTokens, actualTokens, c.TokenCounter)

	slog.Debug("Corpus chunk trimmed",
		"target_tokens", targetTokens,
		"overfetch_tokens", actualTokens,
		"final_bytes", len(text))

	return text
}

// fetchText atomically claims nBytes from the corpus and returns them,
// wrapping around if necessary.
func (c *Corpus) fetchText(nBytes int) string {
	textLen := uint64(len(c.text))

	start := c.offset.Add(uint64(nBytes)) - uint64(nBytes)
	start = start % textLen

	end := start + uint64(nBytes)
	if end <= textLen {
		return c.text[start:end]
	}

	// Wrap around
	var b strings.Builder
	b.WriteString(c.text[start:])
	remaining := nBytes - (int(textLen) - int(start))
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
