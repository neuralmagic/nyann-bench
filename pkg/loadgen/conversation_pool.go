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
	id                  int
	convID              string
	conv                dataset.Conversation
	history             []client.Message
	turn                int
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
}

func newConversationPoolScheduler(g *Generator, poolSize int) *conversationPoolScheduler {
	if poolSize <= 0 {
		poolSize = g.Concurrency
	}
	s := &conversationPoolScheduler{
		g:        g,
		poolSize: poolSize,
		convs:    make(map[int]*pooledConversation, poolSize),
	}

	initial := poolSize
	if state := g.maxReqState.Load(); state != nil && state.limit > 0 && int64(initial) > state.limit {
		initial = int(state.limit)
	}

	slog.Info("Preparing conversation pool", "size", initial)
	step := max(1, initial/4)
	for i := 0; i < initial; i++ {
		s.addConversationWithConv(g.Dataset.NextConversation())
		if (i+1)%step == 0 || i+1 == initial {
			slog.Info("Conversation pool progress", "done", i+1, "total", initial)
		}
	}
	slog.Info("Conversation pool ready", "size", initial)
	return s
}

func (s *conversationPoolScheduler) addConversationWithConv(conv dataset.Conversation) {
	id := s.nextID
	s.nextID++
	pc := &pooledConversation{
		id:     id,
		convID: fmt.Sprintf("pool-c%d", id),
		conv:   conv,
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
	delete(s.convs, pc.id)
	stopped := s.stopped
	needNew := replace && !stopped && len(s.convs) < s.poolSize && s.canCreateConversationLocked()
	s.mu.Unlock()

	if stopped {
		return
	}
	if !replace {
		s.mu.Lock()
		s.convs[pc.id] = pc
		s.pushReadyLocked(pc.id)
		s.mu.Unlock()
		return
	}
	if !needNew {
		return
	}

	conv := s.g.Dataset.NextConversation()

	s.mu.Lock()
	if !s.stopped && len(s.convs) < s.poolSize { // re-check: another goroutine may have filled the slot
		id := s.nextID
		s.nextID++
		s.convs[id] = &pooledConversation{
			id:     id,
			convID: fmt.Sprintf("pool-c%d", id),
			conv:   conv,
		}
		s.pushReadyLocked(id)
	}
	s.mu.Unlock()
}

func (s *conversationPoolScheduler) canCreateConversationLocked() bool {
	state := s.g.maxReqState.Load()
	return state == nil || state.limit <= 0 || state.count.Load() < state.limit
}

func (s *conversationPoolScheduler) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
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

		// Build the pool before starting the stage timer so that
		// conversation generation does not eat into benchmark duration.
		poolSize := stage.ConversationPoolSize
		if poolSize <= 0 {
			poolSize = stage.Concurrency
		}
		scheduler := newConversationPoolScheduler(g, poolSize)
		if onStage != nil {
			onStage(i, stage.Concurrency)
		}

		stageCtx, stageCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer scheduler.stop()
			g.runConversationPoolWorkers(stageCtx, c, scheduler, stage.Concurrency, stage.Rampup)
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

// runConversationPool is used by the single-stage Run path. It creates its own
// scheduler (preparation time counts against the overall run duration there).
func (g *Generator) runConversationPool(ctx context.Context, c *client.Client, concurrency, poolSize int, rampup time.Duration) {
	if concurrency <= 0 {
		return
	}
	if poolSize <= 0 {
		poolSize = concurrency
	}
	scheduler := newConversationPoolScheduler(g, poolSize)
	defer scheduler.stop()
	g.runConversationPoolWorkers(ctx, c, scheduler, concurrency, rampup)
}

// runConversationPoolWorkers runs the worker goroutines against an already-prepared
// scheduler. Called by runConversationPoolStages after pool setup, so that
// conversation generation does not count against the stage timer.
func (g *Generator) runConversationPoolWorkers(ctx context.Context, c *client.Client, scheduler *conversationPoolScheduler, concurrency int, rampup time.Duration) {
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
		Model:     g.Model,
		Messages:  messages,
		Stream:    true,
		MaxTokens: pc.conv.MaxTokens,
		CacheSalt: g.cacheSalt(),
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

	pc.history = append(pc.history, client.Message{
		Role:    "assistant",
		Content: result.Content,
	})
	pc.turn++
	return pc.turn >= len(pc.conv.Turns)
}

func (g *Generator) runPooledCompletionTurn(ctx context.Context, c *client.Client, streamID int, pc *pooledConversation) bool {
	req := &client.CompletionRequest{
		Model:       g.Model,
		Prompt:      pc.conv.Prompt,
		Stream:      true,
		MaxTokens:   pc.conv.MaxTokens,
		Stop:        pc.conv.Stop,
		Temperature: pc.conv.Temperature,
		CacheSalt:   g.cacheSalt(),
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
