package dataset

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/neuralmagic/nyann_poker/pkg/client"
)

// Synthetic generates synthetic conversations with configurable ISL, OSL, and turn count.
type Synthetic struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64
}

func NewSynthetic(isl, osl, turns int, charsPerToken float64) *Synthetic {
	if turns < 1 {
		turns = 1
	}
	return &Synthetic{ISL: isl, OSL: osl, Turns: turns, CharsPerToken: charsPerToken}
}

func (s *Synthetic) turnISL(t int) int {
	if t > 0 && s.SubsequentISL > 0 {
		return s.SubsequentISL
	}
	return s.ISL
}

func (s *Synthetic) NextConversation() Conversation {
	turns := make([][]client.Message, s.Turns)

	var history []client.Message
	for t := 0; t < s.Turns; t++ {
		isl := s.turnISL(t)
		userMsg := client.Message{
			Role:    "user",
			Content: padToTokens(fmt.Sprintf("Turn %d: Please respond with approximately %d tokens.", t+1, s.OSL), isl, s.CharsPerToken),
		}
		history = append(history, userMsg)

		turnMsgs := make([]client.Message, len(history))
		copy(turnMsgs, history)
		turns[t] = turnMsgs

		if t < s.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: padToTokens("This is a simulated assistant response.", s.OSL, s.CharsPerToken),
			})
		}
	}

	return Conversation{Turns: turns, MaxTokens: s.OSL}
}

// padToTokens pads a string with random words to approximate the target token count.
func padToTokens(base string, targetTokens int, charsPerToken float64) string {
	targetChars := int(float64(targetTokens) * charsPerToken)
	if len(base) >= targetChars {
		return base[:targetChars]
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteByte(' ')

	words := []string{"the", "of", "and", "to", "in", "is", "for", "that", "with", "on",
		"are", "be", "this", "from", "or", "an", "by", "as", "but", "not",
		"what", "all", "were", "when", "we", "there", "can", "been", "has", "more"}

	for b.Len() < targetChars {
		b.WriteString(words[rand.Intn(len(words))])
		b.WriteByte(' ')
	}

	return b.String()[:targetChars]
}
