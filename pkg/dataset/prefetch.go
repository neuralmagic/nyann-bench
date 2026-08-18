package dataset

import (
	"sync"

	"github.com/neuralmagic/nyann-bench/pkg/client"
)

// ConversationPrefetchLookahead is how many turns ahead of the current turn
// get materialized in the background, both within a conversation and (via
// Next) across into its successor.
const ConversationPrefetchLookahead = 5

// LazyTurn defers building a turn's cumulative message history until
// Materialize is called; safe to call concurrently or repeatedly.
type LazyTurn struct {
	once   sync.Once
	result []client.Message
	build  func() []client.Message
}

// NewLazyTurn wraps build so it only runs the first time Materialize is called.
func NewLazyTurn(build func() []client.Message) *LazyTurn {
	return &LazyTurn{build: build}
}

// Resolved returns a LazyTurn that is already materialized with msgs. Useful
// for datasets (or tests) that have no work worth deferring.
func Resolved(msgs []client.Message) *LazyTurn {
	lt := &LazyTurn{result: msgs}
	lt.once.Do(func() {})
	return lt
}

// Materialize builds the turn at most once, even under concurrent calls;
// every caller gets the same result.
func (lt *LazyTurn) Materialize() []client.Message {
	lt.once.Do(func() {
		if lt.build != nil {
			lt.result = lt.build()
		}
	})
	return lt.result
}

// ChainedLazyTurns builds n LazyTurns whose build closures share mutable
// state across turns (e.g. cumulative history, a seeded RNG). Turn t's
// build always waits for turn t-1's to finish first, so callers can
// materialize turns in any order or concurrency without corrupting that
// shared state.
func ChainedLazyTurns(n int, build func(t int) []client.Message) []*LazyTurn {
	turns := make([]*LazyTurn, n)
	for t := 0; t < n; t++ {
		t, prev := t, (*LazyTurn)(nil)
		if t > 0 {
			prev = turns[t-1]
		}
		turns[t] = NewLazyTurn(func() []client.Message {
			if prev != nil {
				prev.Materialize()
			}
			return build(t)
		})
	}
	return turns
}

// Bootstrap materializes turns[0] synchronously (no prior in-flight request
// to hide behind) and kicks off turns 1..ConversationPrefetchLookahead in
// the background.
func Bootstrap(turns []*LazyTurn) {
	if len(turns) == 0 {
		return
	}
	turns[0].Materialize()
	for t := 1; t < len(turns) && t <= ConversationPrefetchLookahead; t++ {
		go turns[t].Materialize()
	}
}

// LazyConversation defers building a follow-on conversation until
// Materialize is called, exactly like LazyTurn does for a single turn.
type LazyConversation struct {
	once   sync.Once
	result Conversation
	build  func() Conversation
}

// NewLazyConversation wraps build so it only runs the first time Materialize
// is called.
func NewLazyConversation(build func() Conversation) *LazyConversation {
	return &LazyConversation{build: build}
}

// Materialize builds the conversation at most once, even under concurrent
// calls; every caller gets the same result.
func (lc *LazyConversation) Materialize() Conversation {
	lc.once.Do(func() {
		if lc.build != nil {
			lc.result = lc.build()
		}
	})
	return lc.result
}

// Turn materializes and returns turn i's cumulative message history.
func (c Conversation) Turn(turnIdx int) []client.Message {
	return c.Turns[turnIdx].Materialize()
}

// Successor materializes and returns the dataset-provided next conversation,
// or nil if the dataset left Next unset.
func (c Conversation) Successor() *Conversation {
	if c.Next == nil {
		return nil
	}
	next := c.Next.Materialize()
	return &next
}

// Prefetch kicks off background materialization of the turn
// ConversationPrefetchLookahead turns ahead of turnIdx, crossing into Next's
// turns once the lookahead runs past this conversation's own turns.
func (c Conversation) Prefetch(turnIdx int) {
	lookahead := ConversationPrefetchLookahead
	if lookahead > len(c.Turns) {
		lookahead = len(c.Turns)
	}
	next := turnIdx + lookahead
	if next < len(c.Turns) {
		go c.Turns[next].Materialize()
		return
	}
	if c.Next == nil {
		return
	}
	overflow := next - len(c.Turns)
	go func() {
		nextConv := c.Next.Materialize()
		if overflow < len(nextConv.Turns) {
			nextConv.Turns[overflow].Materialize()
		}
	}()
}
