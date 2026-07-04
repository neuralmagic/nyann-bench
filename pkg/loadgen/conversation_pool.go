package loadgen

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/statsutil"
)

type pooledConversation struct {
	id                  int
	convID              string
	conv                dataset.Conversation
	history             []client.Message
	turn                int
	prevTurnEnd         time.Time // when the previous turn's last token arrived
	lastInterTurnWaitMs float64   // inter-turn wait for the current turn (recorded alongside result)
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

	prefetch chan dataset.Conversation // pre-generated conversations ready for pool insertion
	stopCh   chan struct{}             // signals prefetch worker to exit

	// inter-turn wait tracking: time from turn N's last token to turn N+1's request submission
	interTurnMu      sync.Mutex
	interTurnWaitsMs []float64
}

func (s *conversationPoolScheduler) recordInterTurnWait(ms float64) {
	s.interTurnMu.Lock()
	s.interTurnWaitsMs = append(s.interTurnWaitsMs, ms)
	s.interTurnMu.Unlock()
}

func (s *conversationPoolScheduler) logInterTurnStats() {
	s.interTurnMu.Lock()
	waits := make([]float64, len(s.interTurnWaitsMs))
	copy(waits, s.interTurnWaitsMs)
	s.interTurnMu.Unlock()

	if len(waits) == 0 {
		return
	}
	sort.Float64s(waits)
	slog.Info("inter-turn wait (time between turn N last token → turn N+1 request submitted)",
		"samples", len(waits),
		"p50_ms", statsutil.Percentile(waits, 0.50),
		"p90_ms", statsutil.Percentile(waits, 0.90),
		"p99_ms", statsutil.Percentile(waits, 0.99),
		"max_ms", waits[len(waits)-1],
	)
}

func newConversationPoolScheduler(g *Generator, poolSize int) *conversationPoolScheduler {
	if poolSize <= 0 {
		poolSize = g.Concurrency
	}
	s := &conversationPoolScheduler{
		g:        g,
		poolSize: poolSize,
		convs:    make(map[int]*pooledConversation, poolSize),
		prefetch: make(chan dataset.Conversation, poolSize),
		stopCh:   make(chan struct{}),
	}

	// Generate one base conversation with full tokenizer accuracy. All
	// subsequent conversations are cheap clones with only the first ~512
	// bytes of the first user message randomised — enough to guarantee a
	// unique prefix-cache key per conversation without repeated expensive
	// tokenizer round-trips.
	slog.Info("Generating base conversation for pool")
	base := g.Dataset.NextConversation()

	// Background worker: produces varied conversations from the base so that
	// conversation replacement in complete() is a fast channel receive.
	go func() {
		for {
			conv := varyConversation(base)
			select {
			case s.prefetch <- conv:
			case <-s.stopCh:
				return
			}
		}
	}()

	initial := poolSize
	if state := g.maxReqState.Load(); state != nil && state.limit > 0 && int64(initial) > state.limit {
		initial = int(state.limit)
	}

	slog.Info("Preparing conversation pool", "size", initial)
	step := max(1, initial/4)
	for i := 0; i < initial; i++ {
		s.addConversationWithConv(varyConversation(base))
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

// varyConversation clones base and replaces the first ~512 characters of the
// first user message with random text. This gives each pooled conversation a
// unique prefix-cache key without expensive tokenizer round-trips: only the
// opening block differs, so cross-conversation prefix cache hits are avoided
// while within-conversation turn caching is preserved.
func varyConversation(base dataset.Conversation) dataset.Conversation {
	const variationChars = 512

	turns := make([][]client.Message, len(base.Turns))
	for i, turn := range base.Turns {
		msgs := make([]client.Message, len(turn))
		copy(msgs, turn)
		turns[i] = msgs
	}

	if len(turns) > 0 && len(turns[0]) > 0 {
		orig := turns[0][0].Content
		varLen := min(variationChars, len(orig))
		turns[0][0].Content = randomPrefix(varLen) + orig[varLen:]
	}

	return dataset.Conversation{
		Turns:          turns,
		Prompt:         base.Prompt,
		MaxTokens:      base.MaxTokens,
		Stop:           base.Stop,
		Temperature:    base.Temperature,
		ExpectedAnswer: base.ExpectedAnswer,
	}
}

var (
	poolRNG   = rand.New(rand.NewSource(time.Now().UnixNano()))
	poolRNGMu sync.Mutex
)

// randomPrefix generates n random lowercase ASCII characters (letters and
// spaces) suitable for use as a varied conversation prefix.
func randomPrefix(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz "
	b := make([]byte, n)
	poolRNGMu.Lock()
	for i := range b {
		b[i] = charset[poolRNG.Intn(len(charset))]
	}
	poolRNGMu.Unlock()
	return string(b)
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

	// Receive a pre-generated conversation outside the lock so the CPU-bound
	// text generation (done by the background worker) never blocks the pool.
	var conv dataset.Conversation
	select {
	case conv = <-s.prefetch:
	case <-s.stopCh:
		return
	}

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
	close(s.stopCh) // unblocks the prefetch worker
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

		// Build the pool before starting the stage timer so that
		// conversation generation does not eat into benchmark duration.
		poolSize := stage.ConversationPoolSize
		if poolSize <= 0 {
			poolSize = stage.Concurrency
		}
		scheduler := newConversationPoolScheduler(g, poolSize)

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
				replace := g.runPooledConversationTurn(ctx, c, streamID, pc, scheduler)
				if ctx.Err() != nil {
					return
				}
				scheduler.complete(pc, replace)
			}
		}(i, delay)
	}
	wg.Wait()
	scheduler.logInterTurnStats()
}

// runPooledConversationTurn runs exactly one request from pc.
// It returns true when the conversation should be retired and replaced.
func (g *Generator) runPooledConversationTurn(ctx context.Context, c *client.Client, streamID int, pc *pooledConversation, scheduler *conversationPoolScheduler) bool {
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

	// Record inter-turn wait: time from previous turn's last token to this
	// request being submitted. Captures how long the conversation sat in the
	// pool queue waiting for a concurrency slot — a proxy for the minimum
	// think-time / tool-call latency the system needs at this {C, N} config.
	submitTime := time.Now()
	pc.lastInterTurnWaitMs = 0
	if turnIdx > 0 && !pc.prevTurnEnd.IsZero() {
		pc.lastInterTurnWaitMs = float64(submitTime.Sub(pc.prevTurnEnd).Microseconds()) / 1000.0
		scheduler.recordInterTurnWait(pc.lastInterTurnWaitMs)
	}

	g.trackInFlight(1)
	result := c.ChatStream(ctx, req)
	g.trackInFlight(-1)
	g.trackRequestStatus(result.Err)

	if ctx.Err() != nil && result.Err == nil && result.FinishReason == "" {
		return true
	}

	interTurnWaitMs := pc.lastInterTurnWaitMs
	g.recordWG.Add(1)
	go func() {
		defer g.recordWG.Done()
		g.recordResult(result, streamID, pc.convID, turnIdx, pc.conv, interTurnWaitMs)
	}()

	if result.Err != nil {
		return true
	}

	pc.prevTurnEnd = result.EndTime
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
