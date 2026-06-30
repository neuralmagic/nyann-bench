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
// When a TokenCounter is set, each chunk is tokenized and trimmed to the
// exact target token count before being returned.
type Corpus struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64

	// TokenCounter counts tokens in a string via the server's /tokenize
	// endpoint. When non-nil, nextChunk overfetches text and trims to the
	// exact target token count. When nil, falls back to char-based estimate.
	TokenCounter func(string) (int, error)

	text       string // concatenated corpus text
	baseOffset uint64
	offset     atomic.Uint64
}

func NewCorpus(corpusPath string, isl, osl, turns int, charsPerToken float64, tokenCounter func(string) (int, error)) (*Corpus, error) {
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
	turns := make([][]client.Message, c.Turns)
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
		turns[t] = turnMsgs

		if t < c.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: c.nextChunk(c.OSL),
			})
		}
	}

	return Conversation{Turns: turns, MaxTokens: c.OSL}
}

func (c *Corpus) ConversationAt(index int) Conversation {
	if index < 0 {
		index = 0
	}
	start := c.baseOffset + uint64(index*c.conversationStride())
	return c.conversationFromOffset(start)
}

func (c *Corpus) conversationFromOffset(start uint64) Conversation {
	turns := make([][]client.Message, c.Turns)
	var history []client.Message
	cursor := start

	for t := 0; t < c.Turns; t++ {
		chunk := c.chunkAt(cursor, c.turnISL(t))
		cursor += uint64(c.chunkStride(c.turnISL(t)))
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
			cursor += uint64(c.chunkStride(c.OSL))
		}
	}

	return Conversation{Turns: turns, MaxTokens: c.OSL}
}

func (c *Corpus) conversationStride() int {
	total := 0
	for t := 0; t < c.Turns; t++ {
		total += c.chunkStride(c.turnISL(t))
		if t < c.Turns-1 {
			total += c.chunkStride(c.OSL)
		}
	}
	if total < 1 {
		return 1
	}
	return total
}

func (c *Corpus) estimatedBytes(targetTokens int) int {
	estimatedBytes := int(float64(targetTokens) * c.CharsPerToken)
	if estimatedBytes < 0 {
		return 0
	}
	return estimatedBytes
}

func (c *Corpus) chunkStride(targetTokens int) int {
	stride := c.estimatedBytes(targetTokens)
	if c.TokenCounter != nil {
		stride *= 2
	}
	if stride < 1 {
		return 1
	}
	return stride
}

// nextChunk returns targetTokens worth of text from the corpus, advancing the
// shared offset. When a TokenCounter is available, the chunk is overfetched
// and trimmed to the exact token count; otherwise falls back to char estimate.
func (c *Corpus) nextChunk(targetTokens int) string {
	estimatedBytes := c.estimatedBytes(targetTokens)

	if c.TokenCounter == nil {
		return c.fetchText(estimatedBytes)
	}

	// Overfetch 2x, then binary-search for the exact trim point.
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

	// Binary search for the trim point that yields ~targetTokens.
	lo, hi := 0, len(text)
	for hi-lo > 16 {
		mid := (lo + hi) / 2
		count, err := c.TokenCounter(text[:mid])
		if err != nil {
			break
		}
		if count < targetTokens {
			lo = mid
		} else {
			hi = mid
		}
	}

	slog.Debug("Corpus chunk trimmed",
		"target_tokens", targetTokens,
		"overfetch_tokens", actualTokens,
		"final_bytes", hi)

	return text[:hi]
}

// fetchText atomically claims nBytes from the corpus and returns them,
// wrapping around if necessary.
func (c *Corpus) fetchText(nBytes int) string {
	if nBytes < 0 {
		nBytes = 0
	}
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

func (c *Corpus) chunkAt(start uint64, targetTokens int) string {
	estimatedBytes := c.estimatedBytes(targetTokens)
	if c.TokenCounter == nil {
		return c.fetchTextAt(start, estimatedBytes)
	}

	text := c.fetchTextAt(start, estimatedBytes*2)

	actualTokens, err := c.TokenCounter(text)
	if err != nil {
		slog.Debug("TokenCounter failed, using char estimate", "error", err)
		if estimatedBytes <= len(text) {
			return text[:estimatedBytes]
		}
		return text
	}

	if actualTokens < targetTokens && actualTokens > 0 {
		bytesPerToken := float64(len(text)) / float64(actualTokens)
		extra := int(float64(targetTokens-actualTokens)*bytesPerToken) * 2
		text += c.fetchTextAt(start+uint64(len(text)), extra)
		actualTokens, err = c.TokenCounter(text)
		if err != nil || actualTokens <= targetTokens {
			return text
		}
	} else if actualTokens <= targetTokens {
		return text
	}

	lo, hi := 0, len(text)
	for hi-lo > 16 {
		mid := (lo + hi) / 2
		count, err := c.TokenCounter(text[:mid])
		if err != nil {
			break
		}
		if count < targetTokens {
			lo = mid
		} else {
			hi = mid
		}
	}

	return text[:hi]
}

func (c *Corpus) fetchTextAt(start uint64, nBytes int) string {
	if nBytes < 0 {
		nBytes = 0
	}
	textLen := uint64(len(c.text))
	start = start % textLen

	end := start + uint64(nBytes)
	if end <= textLen {
		return c.text[start:end]
	}

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
