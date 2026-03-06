package dataset

import "github.com/neuralmagic/nyann_poker/pkg/client"

// Conversation is a multi-turn conversation with a max_tokens hint per turn.
type Conversation struct {
	Turns     [][]client.Message // Messages for each turn (cumulative history)
	MaxTokens int                // Requested max output tokens per turn (0 = no limit)
}

// Dataset provides conversations for the load generator.
type Dataset interface {
	// NextConversation returns a conversation (one or more turns).
	NextConversation() Conversation
}
