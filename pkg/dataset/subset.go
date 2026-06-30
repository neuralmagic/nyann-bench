package dataset

import (
	"fmt"
	"math/rand"
	"sync/atomic"
)

const DefaultPromptSubsetSeed int64 = 42

// IndexedDataset can replay a conversation by stable index.
type IndexedDataset interface {
	Dataset
	ConversationAt(index int) Conversation
}

// PromptSubset cycles over a fixed set of conversation indices.
type PromptSubset struct {
	inner   IndexedDataset
	indices []int
	idx     atomic.Uint64
}

type sizedDataset interface {
	Len() int
}

func NewPromptSubset(inner IndexedDataset, count int, seed int64) (*PromptSubset, error) {
	if count <= 0 {
		return nil, fmt.Errorf("prompt_subset must be > 0, got %d", count)
	}

	sourceLen := count
	if sized, ok := inner.(sizedDataset); ok {
		sourceLen = sized.Len()
		if count > sourceLen {
			count = sourceLen
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("prompt_subset leaves no prompts")
	}

	rng := rand.New(rand.NewSource(seed))
	perm := rng.Perm(sourceLen)
	return &PromptSubset{inner: inner, indices: perm[:count]}, nil
}

func (p *PromptSubset) Len() int {
	return len(p.indices)
}

func (p *PromptSubset) Partition(workerID, numWorkers int) {
	if numWorkers <= 0 {
		panic("Partition: numWorkers must be > 0")
	}
	if workerID < 0 || workerID >= numWorkers {
		panic(fmt.Sprintf("Partition: workerID %d out of range [0, %d)", workerID, numWorkers))
	}

	n := len(p.indices)
	base := n / numWorkers
	remainder := n % numWorkers

	start := workerID*base + min(workerID, remainder)
	size := base
	if workerID < remainder {
		size++
	}

	p.indices = p.indices[start : start+size]
	p.idx.Store(0)
}

func (p *PromptSubset) NextConversation() Conversation {
	idx := p.idx.Add(1) - 1
	return p.inner.ConversationAt(p.indices[idx%uint64(len(p.indices))])
}
