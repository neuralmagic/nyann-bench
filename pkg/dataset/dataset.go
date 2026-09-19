package dataset

import "github.com/neuralmagic/nyann-bench/pkg/client"

// Conversation is a multi-turn conversation with a max_tokens hint per turn.
type Conversation struct {
	Turns          [][]client.Message // Messages for each turn (cumulative history)
	System         string             // Leading system message on every turn, if non-empty
	Prompt         string             // If non-empty, use completions API instead of chat (single-turn only)
	MaxTokens      int                // Requested max output tokens per turn (0 = no limit)
	Stop           []string           // Stop sequences for completions API
	Temperature    *float64           // Sampling temperature (nil = server default)
	ExpectedAnswer string             // If non-empty, evaluate the model's response against this
}

// Dataset provides conversations for the load generator.
type Dataset interface {
	// NextConversation returns a conversation (one or more turns).
	NextConversation() Conversation
}

// WithSystemPrompt gives every conversation the same leading system message.
type WithSystemPrompt struct {
	Inner  Dataset
	Prompt string
}

func (d *WithSystemPrompt) NextConversation() Conversation {
	conv := d.Inner.NextConversation()
	conv.System = d.Prompt
	return conv
}
