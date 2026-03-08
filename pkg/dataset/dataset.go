package dataset

import "github.com/neuralmagic/nyann_poker/pkg/client"

// Conversation is a multi-turn conversation with a max_tokens hint per turn.
type Conversation struct {
	Turns          [][]client.Message // Messages for each turn (cumulative history)
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
