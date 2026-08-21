package loadgen

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
)

type pooledConversation struct {
	id           int
	convID       string
	conv         dataset.Conversation
	materialized bool
	history      []client.Message
	turn         int
}

type conversationPoolScheduler struct {
	g        *Generator
	poolSize int

	mu      sync.Mutex
	ready   []int
	head    int
	convs   map[int]*pooledConversation
	nextID  int
	stopped bool

	// backupPool holds conversations built ahead of need to keep NextConversation() off the hot path.
	backupPool chan dataset.Conversation
	stopCh     chan struct{}
}

// newConversationPoolScheduler builds the pool and blocks until backupPool is
// prewarmed, so warm-up cost is paid before the caller's measured window starts.
func newConversationPoolScheduler(ctx context.Context, g *Generator, poolSize int) *conversationPoolScheduler {
	if poolSize <= 0 {
		poolSize = g.Concurrency
	}
	s := &conversationPoolScheduler{
		g:          g,
		poolSize:   poolSize,
		convs:      make(map[int]*pooledConversation, poolSize),
		backupPool: make(chan dataset.Conversation, poolSize),
		stopCh:     make(chan struct{}),
	}

	initial := poolSize
	if state := g.maxReqState.Load(); state != nil && state.limit > 0 && int64(initial) > state.limit {
		initial = int(state.limit)
	}
	for i := 0; i < initial; i++ {
		s.addConversationSlotLocked()
	}

	s.prewarmBackupPool(ctx, initial)
	return s
}

// prewarmBackupPool builds n conversations in parallel and blocks until all
// are queued in backupPool or ctx is done.
func (s *conversationPoolScheduler) prewarmBackupPool(ctx context.Context, n int) {
	if n <= 0 {
		return
	}
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			conv := s.g.Dataset.NextConversation()
			select {
			case s.backupPool <- conv:
			case <-ctx.Done():
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// produceBackupAsync builds one replacement conversation off the hot path and queues it in backupPool.
func (s *conversationPoolScheduler) produceBackupAsync() {
	go func() {
		conv := s.g.Dataset.NextConversation()
		select {
		case s.backupPool <- conv:
		case <-s.stopCh:
		}
	}()
}

func (s *conversationPoolScheduler) addConversationSlotLocked() {
	id := s.nextID
	s.nextID++
	pc := &pooledConversation{
		id:     id,
		convID: fmt.Sprintf("pool-c%d", id),
	}
	s.convs[id] = pc
	s.pushReadyLocked(id)
}

func (s *conversationPoolScheduler) pushReadyLocked(id int) {
	s.ready = append(s.ready, id)
}

func (s *conversationPoolScheduler) popReadyLocked() (*pooledConversation, bool) {
	if s.head >= len(s.ready) {
		s.ready = s.ready[:0]
		s.head = 0
		return nil, false
	}
	id := s.ready[s.head]
	s.head++
	if s.head > 1024 && s.head*2 >= len(s.ready) {
		copy(s.ready, s.ready[s.head:])
		s.ready = s.ready[:len(s.ready)-s.head]
		s.head = 0
	}
	pc, ok := s.convs[id]
	return pc, ok
}

func (s *conversationPoolScheduler) reserveRequestLocked() bool {
	state := s.g.maxReqState.Load()
	if state == nil || state.limit <= 0 {
		return true
	}
	cur := state.count.Load()
	if cur >= state.limit {
		state.once.Do(func() { close(state.done) })
		return false
	}
	next := state.count.Add(1)
	if next >= state.limit {
		state.once.Do(func() { close(state.done) })
	}
	return true
}

func (s *conversationPoolScheduler) next() (*pooledConversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped || !s.reserveRequestLocked() {
		return nil, false
	}
	pc, ok := s.popReadyLocked()
	if !ok {
		return nil, false
	}
	return pc, true
}

func (s *conversationPoolScheduler) complete(pc *pooledConversation, replace bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.convs, pc.id)
	if s.stopped {
		return
	}
	if replace {
		if len(s.convs) < s.poolSize && s.canCreateConversationLocked() {
			s.addConversationSlotLocked()
			s.produceBackupAsync()
		}
		return
	}
	s.convs[pc.id] = pc
	s.pushReadyLocked(pc.id)
}

func (s *conversationPoolScheduler) canCreateConversationLocked() bool {
	state := s.g.maxReqState.Load()
	return state == nil || state.limit <= 0 || state.count.Load() < state.limit
}

func (s *conversationPoolScheduler) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	close(s.stopCh)
}

func (g *Generator) runConversationPoolStages(ctx context.Context, stages []Stage, onStage func(index, concurrency int), onBarrier func(index int)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.stopFunc = cancel

	c := client.New(g.Target)

	for i, stage := range stages {
		if ctx.Err() != nil {
			break
		}
		if stage.Barrier {
			if onBarrier != nil {
				onBarrier(i)
			}
			continue
		}

		state := &maxRequestsState{
			limit: int64(stage.MaxRequests),
			done:  make(chan struct{}),
		}
		g.maxReqState.Store(state)
		if onStage != nil {
			onStage(i, stage.Concurrency)
		}

		stageCtx, stageCancel := context.WithCancel(ctx)

		// Prewarm before the stage timer below starts, so warm-up cost isn't stolen from the stage's duration.
		scheduler := newConversationPoolScheduler(stageCtx, g, stage.ConversationPoolSize)

		done := make(chan struct{})
		go func() {
			defer close(done)
			g.runConversationPool(stageCtx, c, scheduler, stage.Concurrency, stage.Rampup)
		}()

		if stage.MaxRequests > 0 {
			select {
			case <-ctx.Done():
			case <-done:
			case <-time.After(stage.Duration):
				slog.Warn("Stage timed out before all requests completed",
					"dispatched", state.count.Load(),
					"target", stage.MaxRequests)
				stageCancel()
				<-done
			}
		} else {
			select {
			case <-ctx.Done():
			case <-done:
			case <-time.After(stage.Duration):
				stageCancel()
				<-done
			}
		}
		stageCancel()
	}

	g.recordWG.Wait()
}

func (g *Generator) runConversationPool(ctx context.Context, c *client.Client, scheduler *conversationPoolScheduler, concurrency int, rampup time.Duration) {
	defer scheduler.stop()
	if concurrency <= 0 {
		return
	}

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		delay := time.Duration(0)
		if rampup > 0 && concurrency > 1 {
			delay = rampup * time.Duration(i) / time.Duration(concurrency-1)
		}

		wg.Add(1)
		go func(streamID int, delay time.Duration) {
			defer wg.Done()
			if delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}

			for {
				if ctx.Err() != nil {
					return
				}
				pc, ok := scheduler.next()
				if !ok {
					return
				}
				if !pc.materialized {
					select {
					case conv := <-scheduler.backupPool:
						pc.conv = conv
					default:
						pc.conv = g.Dataset.NextConversation()
					}
					pc.materialized = true
				}
				if ctx.Err() != nil {
					return
				}
				replace := g.runPooledConversationTurn(ctx, c, streamID, pc)
				if ctx.Err() != nil {
					return
				}
				scheduler.complete(pc, replace)
			}
		}(i, delay)
	}
	wg.Wait()
}

// runPooledConversationTurn runs exactly one request from pc.
// It returns true when the conversation should be retired and replaced.
func (g *Generator) runPooledConversationTurn(ctx context.Context, c *client.Client, streamID int, pc *pooledConversation) bool {
	if pc.conv.Prompt != "" {
		return g.runPooledCompletionTurn(ctx, c, streamID, pc)
	}
	if pc.turn >= len(pc.conv.Turns) {
		return true
	}

	turnIdx := pc.turn
	prebuilt := pc.conv.Turns[turnIdx]
	if len(prebuilt) == 0 {
		return true
	}
	userMsg := prebuilt[len(prebuilt)-1]
	pc.history = append(pc.history, userMsg)

	messages := make([]client.Message, len(pc.history))
	copy(messages, pc.history)

	req := &client.Request{
		Model:         g.Model,
		Messages:      messages,
		Stream:        true,
		StreamOptions: g.streamOptions(),
		MaxTokens:     pc.conv.MaxTokens,
		CacheSalt:     g.cacheSalt(),
	}

	g.trackInFlight(1)
	result := c.ChatStream(ctx, req)
	g.trackInFlight(-1)
	g.trackRequestStatus(result.Err)

	if ctx.Err() != nil && result.Err == nil && result.FinishReason == "" {
		return true
	}

	g.recordWG.Add(1)
	go func() {
		defer g.recordWG.Done()
		g.recordResult(result, streamID, pc.convID, turnIdx, pc.conv)
	}()

	if result.Err != nil {
		return true
	}

	// Pool mode models the complete generated KV working set, so replay both
	// reasoning and visible content. Evaluation still uses result.Content.
	pc.history = append(pc.history, client.Message{
		Role:    "assistant",
		Content: result.GeneratedText,
	})
	pc.turn++
	return pc.turn >= len(pc.conv.Turns)
}

func (g *Generator) runPooledCompletionTurn(ctx context.Context, c *client.Client, streamID int, pc *pooledConversation) bool {
	req := &client.CompletionRequest{
		Model:         g.Model,
		Prompt:        pc.conv.Prompt,
		Stream:        true,
		StreamOptions: g.streamOptions(),
		MaxTokens:     pc.conv.MaxTokens,
		Stop:          pc.conv.Stop,
		Temperature:   pc.conv.Temperature,
		CacheSalt:     g.cacheSalt(),
	}

	g.trackInFlight(1)
	result := c.CompletionStream(ctx, req)
	g.trackInFlight(-1)
	g.trackRequestStatus(result.Err)

	if ctx.Err() != nil && result.Err == nil && result.FinishReason == "" {
		return true
	}

	g.recordWG.Add(1)
	go func() {
		defer g.recordWG.Done()
		g.recordResult(result, streamID, pc.convID, 0, pc.conv)
	}()
	return true
}
