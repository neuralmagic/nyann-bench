package dataset

import (
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// tokenChunkSize is the byte size of each chunk sent to /tokenize during
// corpus pre-tokenization. Kept small enough that any server handles it
// quickly, large enough to amortize HTTP overhead.
const tokenChunkSize = 8192

// Corpus generates conversations by sliding a window over real text files.
// When a TokenCounter is provided at construction, the corpus is
// pre-tokenized so that nextChunk slices by exact token boundaries instead
// of character estimates.
type Corpus struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64

	text   string // concatenated corpus text
	offset atomic.Uint64

	// tokenOffsets maps byte offsets to cumulative token counts.
	// tokenOffsets[i] = {ByteOffset, CumulativeTokens} for the i-th chunk boundary.
	// Populated by preTokenize; nil when no TokenCounter was provided.
	tokenOffsets []tokenOffset
}

type tokenOffset struct {
	Byte   int // byte position in corpus text
	Tokens int // cumulative token count up to this byte position
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

	c := &Corpus{ISL: isl, OSL: osl, Turns: turns, CharsPerToken: charsPerToken, text: text}
	// Start at a random offset so multiple workers don't read the same text
	c.offset.Store(uint64(rand.Intn(len(text))))

	if tokenCounter != nil {
		if err := c.preTokenize(tokenCounter); err != nil {
			slog.Warn("Corpus pre-tokenization failed, falling back to char estimate", "error", err)
		}
	}

	return c, nil
}

// preTokenize splits the corpus into fixed-size chunks, tokenizes each via
// the provided counter, and builds a cumulative token-offset index.
func (c *Corpus) preTokenize(counter func(string) (int, error)) error {
	n := len(c.text)
	// +1 for the final entry at len(text)
	offsets := make([]tokenOffset, 0, n/tokenChunkSize+2)
	offsets = append(offsets, tokenOffset{Byte: 0, Tokens: 0})

	cumTokens := 0
	nChunks := (n + tokenChunkSize - 1) / tokenChunkSize
	for start := 0; start < n; start += tokenChunkSize {
		end := start + tokenChunkSize
		if end > n {
			end = n
		}
		count, err := counter(c.text[start:end])
		if err != nil {
			return fmt.Errorf("tokenizing chunk at offset %d: %w", start, err)
		}
		cumTokens += count
		offsets = append(offsets, tokenOffset{Byte: end, Tokens: cumTokens})

		chunkIdx := start/tokenChunkSize + 1
		if chunkIdx%50 == 0 || chunkIdx == nChunks {
			slog.Debug("Pre-tokenize progress",
				"chunk", fmt.Sprintf("%d/%d", chunkIdx, nChunks),
				"bytes", end,
				"cumulative_tokens", cumTokens,
				"chunk_chars_per_token", fmt.Sprintf("%.2f", float64(end-start)/float64(count)))
		}
	}

	c.tokenOffsets = offsets
	slog.Info("Corpus pre-tokenized",
		"bytes", n,
		"tokens", cumTokens,
		"effective_chars_per_token", fmt.Sprintf("%.2f", float64(n)/float64(cumTokens)))
	return nil
}

// bytesForTokens returns how many bytes from position startByte correspond to
// targetTokens tokens, using the pre-built token offset index.
func (c *Corpus) bytesForTokens(startByte, targetTokens int) int {
	n := len(c.text)
	startByte = startByte % n

	// Find cumulative token count at startByte by interpolating between
	// the two surrounding index entries.
	startTokens := c.tokensAtByte(startByte)
	goalTokens := startTokens + targetTokens

	// Binary search for the byte offset where cumulative tokens reaches goalTokens.
	// Search within the offset table first.
	idx := sort.Search(len(c.tokenOffsets), func(i int) bool {
		return c.tokenOffsets[i].Tokens >= goalTokens
	})

	if idx >= len(c.tokenOffsets) {
		// Target exceeds corpus length — return remaining bytes to end.
		return n - startByte
	}

	// Interpolate within the chunk to get a tighter estimate.
	var endByte int
	if idx == 0 {
		endByte = 0
	} else {
		prev := c.tokenOffsets[idx-1]
		cur := c.tokenOffsets[idx]
		chunkTokens := cur.Tokens - prev.Tokens
		if chunkTokens == 0 {
			endByte = cur.Byte
		} else {
			overshoot := goalTokens - prev.Tokens
			chunkBytes := cur.Byte - prev.Byte
			endByte = prev.Byte + chunkBytes*overshoot/chunkTokens
		}
	}

	byteLen := endByte - startByte
	if byteLen < 0 {
		byteLen = 0
	}
	return byteLen
}

// tokensAtByte returns the interpolated cumulative token count at a byte position.
func (c *Corpus) tokensAtByte(pos int) int {
	idx := sort.Search(len(c.tokenOffsets), func(i int) bool {
		return c.tokenOffsets[i].Byte >= pos
	})
	if idx >= len(c.tokenOffsets) {
		return c.tokenOffsets[len(c.tokenOffsets)-1].Tokens
	}
	if c.tokenOffsets[idx].Byte == pos {
		return c.tokenOffsets[idx].Tokens
	}
	if idx == 0 {
		return 0
	}
	prev := c.tokenOffsets[idx-1]
	cur := c.tokenOffsets[idx]
	chunkBytes := cur.Byte - prev.Byte
	if chunkBytes == 0 {
		return prev.Tokens
	}
	frac := float64(pos-prev.Byte) / float64(chunkBytes)
	return prev.Tokens + int(frac*float64(cur.Tokens-prev.Tokens))
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

// nextChunk returns targetTokens worth of text from the corpus, advancing the
// shared offset. When the token-offset index is available, slicing is exact;
// otherwise it falls back to the character-based estimate.
func (c *Corpus) nextChunk(targetTokens int) string {
	if c.tokenOffsets == nil {
		nBytes := int(float64(targetTokens) * c.CharsPerToken)
		slog.Debug("Corpus chunk (char-based)",
			"target_tokens", targetTokens,
			"chars", nBytes,
			"chars_per_token", c.CharsPerToken)
		return c.fetchText(nBytes)
	}

	// Use the char-based estimate to claim a region, then adjust the
	// returned slice length using the token-offset index. The offset
	// advances by the estimate — close enough to avoid systematic drift.
	estimatedBytes := int(float64(targetTokens) * c.CharsPerToken)
	textLen := uint64(len(c.text))

	start := c.offset.Add(uint64(estimatedBytes)) - uint64(estimatedBytes)
	start = start % textLen

	nBytes := c.bytesForTokens(int(start), targetTokens)
	slog.Debug("Corpus chunk (token-indexed)",
		"target_tokens", targetTokens,
		"start_byte", start,
		"estimated_bytes", estimatedBytes,
		"actual_bytes", nBytes,
		"ratio", fmt.Sprintf("%.2f", float64(nBytes)/float64(estimatedBytes)))
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
