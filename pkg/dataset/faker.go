package dataset

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// Faker generates conversations using gofakeit for realistic, diverse text.
type Faker struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64
	TokenCounter  TokenCounter
	seq 		  atomic.Uint64
}

func NewFaker(isl, osl, turns int, charsPerToken float64) *Faker {
	return NewFakerWithTokenCounter(isl, osl, turns, charsPerToken, nil)
}

func NewFakerWithTokenCounter(isl, osl, turns int, charsPerToken float64, tokenCounter TokenCounter) *Faker {
	if turns < 1 {
		turns = 1
	}
	return &Faker{ISL: isl, OSL: osl, Turns: turns, CharsPerToken: charsPerToken, TokenCounter: tokenCounter}
}

func (f *Faker) turnISL(t int) int {
	if t > 0 && f.SubsequentISL > 0 {
		return f.SubsequentISL
	}
	return f.ISL
}

// NextConversation is the cold-start entry point, called once per stream or
// pool slot; later conversations in that chain are drawn via Next/Successor.
func (f *Faker) NextConversation() Conversation {
	return f.buildConversation(true)
}

// buildConversation generates one conversation; safe to call concurrently
// (each call reserves its own seed via f.seq.Add(1)). If bootstrap is true,
// turn 0 is materialized now and turns 1..ConversationPrefetchLookahead are
// kicked off in the background; otherwise every turn stays lazy until a
// caller (e.g. the pool scheduler's Prefetch walk) materializes it.
func (f *Faker) buildConversation(bootstrap bool) Conversation {
	seed := f.seq.Add(1)
	faker := gofakeit.New(seed)
	var history []client.Message

	turns := ChainedLazyTurns(f.Turns, func(t int) []client.Message {
		prompt := f.generatePrompt(faker, t)
		userMsg := client.Message{
			Role:    "user",
			Content: f.padWithFaker(faker, prompt, f.turnISL(t)),
		}
		history = append(history, userMsg)

		turnMsgs := make([]client.Message, len(history))
		copy(turnMsgs, history)

		if t < f.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: f.padWithFaker(faker, "Here is my response.", f.OSL),
			})
		}
		return turnMsgs
	})

	conv := Conversation{Turns: turns, MaxTokens: f.OSL}
	conv.Next = NewLazyConversation(func() Conversation { return f.buildConversation(false) })

	if bootstrap {
		Bootstrap(turns)
	}

	return conv
}

func (f *Faker) padWithFaker(faker *gofakeit.Faker, base string, targetTokens int) string {
	return padWithFaker(faker, base, targetTokens, f.CharsPerToken, f.TokenCounter)
}

// generatePrompt creates a diverse, realistic prompt.
func (f *Faker) generatePrompt(faker *gofakeit.Faker, turn int) string {
	templates := []func() string{
		func() string {
			return fmt.Sprintf("Hello, my name is %s %s from %s, %s. %s",
				faker.FirstName(), faker.LastName(), faker.City(), faker.Country(),
				faker.Question())
		},
		func() string {
			return fmt.Sprintf("I'm working on a project about %s. Can you explain %s in detail?",
				faker.BuzzWord(), faker.HipsterWord())
		},
		func() string {
			return fmt.Sprintf("As a %s at %s, I need help with the following: %s",
				faker.JobTitle(), faker.Company(), faker.HackerPhrase())
		},
		func() string {
			return fmt.Sprintf("Write a %s about %s who lives in %s. The story should explore themes of %s.",
				faker.RandomString([]string{"story", "poem", "essay", "letter", "speech"}),
				faker.Name(), faker.City(),
				faker.BuzzWord())
		},
		func() string {
			return fmt.Sprintf("Please analyze this situation: %s. Consider the perspective of someone from %s who works as a %s.",
				faker.Sentence(12), faker.Country(), faker.JobTitle())
		},
		func() string {
			return fmt.Sprintf("Translate the following concept into simple terms: %s. %s",
				faker.HackerPhrase(), faker.Question())
		},
		func() string {
			return fmt.Sprintf("I'd like to discuss %s. Specifically, %s How does this relate to %s?",
				faker.Word(), faker.Question(), faker.BuzzWord())
		},
	}

	idx := faker.IntN(len(templates))
	return templates[idx]()
}

// padWithFaker pads text to target token count using gofakeit paragraphs.
func padWithFaker(faker *gofakeit.Faker, base string, targetTokens int, charsPerToken float64, tokenCounter TokenCounter) string {
	targetChars := int(float64(targetTokens) * charsPerToken)
	if len(base) >= targetChars {
		trimmed, _ := trimToTokenBudget(base[:targetChars], targetTokens, tokenCounter)
		return trimmed
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteByte(' ')

	for b.Len() < targetChars {
		b.WriteString(faker.Sentence(faker.IntN(12) + 5))
		b.WriteByte(' ')
	}

	trimmed, _ := trimToTokenBudget(b.String()[:targetChars], targetTokens, tokenCounter)
	return trimmed
}
