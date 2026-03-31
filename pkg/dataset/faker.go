package dataset

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/brianvoe/gofakeit/v7"
	"github.com/neuralmagic/nyann_poker/pkg/client"
)

// Faker generates conversations using gofakeit for realistic, diverse text.
type Faker struct {
	ISL           int
	SubsequentISL int // ISL for turns > 0 (0 = use ISL)
	OSL           int
	Turns         int
	CharsPerToken float64
	seq           atomic.Uint64
}

func NewFaker(isl, osl, turns int, charsPerToken float64) *Faker {
	if turns < 1 {
		turns = 1
	}
	return &Faker{ISL: isl, OSL: osl, Turns: turns, CharsPerToken: charsPerToken}
}

func (f *Faker) turnISL(t int) int {
	if t > 0 && f.SubsequentISL > 0 {
		return f.SubsequentISL
	}
	return f.ISL
}

func (f *Faker) NextConversation() Conversation {
	seed := f.seq.Add(1)
	faker := gofakeit.New(seed)

	turns := make([][]client.Message, f.Turns)
	var history []client.Message

	for t := 0; t < f.Turns; t++ {
		prompt := f.generatePrompt(faker, t)
		userMsg := client.Message{
			Role:    "user",
			Content: padWithFaker(faker, prompt, f.turnISL(t), f.CharsPerToken),
		}
		history = append(history, userMsg)

		turnMsgs := make([]client.Message, len(history))
		copy(turnMsgs, history)
		turns[t] = turnMsgs

		if t < f.Turns-1 {
			history = append(history, client.Message{
				Role:    "assistant",
				Content: padWithFaker(faker, "Here is my response.", f.OSL, f.CharsPerToken),
			})
		}
	}

	return Conversation{Turns: turns, MaxTokens: f.OSL}
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
func padWithFaker(faker *gofakeit.Faker, base string, targetTokens int, charsPerToken float64) string {
	targetChars := int(float64(targetTokens) * charsPerToken)
	if len(base) >= targetChars {
		return base[:targetChars]
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteByte(' ')

	for b.Len() < targetChars {
		b.WriteString(faker.Sentence(faker.IntN(12) + 5))
		b.WriteByte(' ')
	}

	return b.String()[:targetChars]
}
