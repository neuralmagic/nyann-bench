package dataset

// Conversation is a multi-turn conversation with a max_tokens hint per turn.
type Conversation struct {
	Turns          []*LazyTurn // Per-turn cumulative history, built lazily on demand
	Prompt         string      // If non-empty, use completions API instead of chat (single-turn only)
	MaxTokens      int         // Requested max output tokens per turn (0 = no limit)
	Stop           []string    // Stop sequences for completions API
	Temperature    *float64    // Sampling temperature (nil = server default)
	ExpectedAnswer string      // If non-empty, evaluate the model's response against this

	// Next defers building the successor conversation; see Prefetch.
	// Most datasets have no need for it and leave it nil.
	Next *LazyConversation
}

// Dataset provides conversations for the load generator.
type Dataset interface {
	// NextConversation returns a conversation (one or more turns).
	NextConversation() Conversation
}
