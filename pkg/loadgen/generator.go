package loadgen

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	mathrand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/client"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/dataset"
	"github.com/neuralmagic/nyann-bench/pkg/eval"
	"github.com/neuralmagic/nyann-bench/pkg/metrics"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// Mode determines how requests are scheduled.
type Mode string

const (
	// ModeConcurrent sends requests from a fixed number of streams.
	// Each stream sends the next request immediately on completion.
	ModeConcurrent Mode = "concurrent"

	// ModeConstant sends requests at a fixed rate (requests/sec).
	// Arrival times are deterministic (evenly spaced).
	ModeConstant Mode = "constant"

	// ModePoisson sends requests at a target rate with Poisson-distributed
	// inter-arrival times (exponential gaps). Models realistic traffic.
	ModePoisson Mode = "poisson"
)

// maxRequestsState holds per-stage state for the max_requests feature.
// Swapped atomically between stages so goroutines from previous stages
// don't race with the new stage's initialization.
type maxRequestsState struct {
	limit int64
	count atomic.Int64
	done  chan struct{}
	once  sync.Once
}

type Generator struct {
	Target      string
	Model       string
	Mode        Mode
	Concurrency int           // For ModeConcurrent: number of streams
	Rate        float64       // For ModeConstant/ModePoisson: requests per second
	MaxInFlight int           // For ModeConstant/ModePoisson: cap on concurrent requests (0 = unlimited)
	Rampup      time.Duration // Stagger stream starts (concurrent) or ramp rate (constant/poisson)
	Duration    time.Duration
	Dataset     dataset.Dataset
	Recorder    *recorder.Recorder
	CacheSalt *config.CacheSalt // Prefix cache isolation (nil = disabled)
	Metrics     *metrics.Metrics // Optional Prometheus metrics (nil = disabled)

	recorderPtr atomic.Pointer[recorder.Recorder] // swappable recorder for warmup→main transition
	recordWG    sync.WaitGroup // tracks in-flight recordResult goroutines
	inFlight    atomic.Int64
	evalCount   atomic.Int64
	evalCorrect atomic.Int64

	maxReqState atomic.Pointer[maxRequestsState]
}

// streamPool manages a resizable pool of concurrent streams.
// Streams are added/removed dynamically without tearing down existing ones.
type streamPool struct {
	g       *Generator
	c       *client.Client
	mu      sync.Mutex
	streams map[int]context.CancelFunc // streamID -> cancel
	nextID  int
	wg      sync.WaitGroup
}

func newStreamPool(g *Generator, c *client.Client) *streamPool {
	return &streamPool{
		g:       g,
		c:       c,
		streams: make(map[int]context.CancelFunc),
	}
}

// Resize adjusts the pool to the target concurrency.
// New streams start immediately (or staggered over rampup if > 0);
// excess streams are cancelled (they finish their current in-flight request, then exit).
func (p *streamPool) Resize(ctx context.Context, target int, rampup time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := len(p.streams)
	newCount := target - current

	// Add streams
	idx := 0
	for current < target {
		id := p.nextID
		p.nextID++
		streamCtx, cancel := context.WithCancel(ctx)
		p.streams[id] = cancel

		// Compute stagger delay: spread new streams evenly over rampup
		delay := time.Duration(0)
		if rampup > 0 && newCount > 1 {
			delay = rampup * time.Duration(idx) / time.Duration(newCount-1)
		}

		p.wg.Add(1)
		go func(sid int, sctx context.Context, d time.Duration) {
			defer p.wg.Done()
			p.g.runStream(sctx, p.c, sid, d)
			p.mu.Lock()
			delete(p.streams, sid)
			p.mu.Unlock()
		}(id, streamCtx, delay)
		current++
		idx++
	}

	// Remove excess streams (cancel the most recently added)
	for current > target {
		// Find any stream to cancel
		for id, cancel := range p.streams {
			cancel()
			delete(p.streams, id)
			break
		}
		current--
	}
}

// Wait blocks until all streams have exited.
func (p *streamPool) Wait() {
	p.wg.Wait()
}

// Stop cancels all streams and waits for them to finish.
func (p *streamPool) Stop() {
	p.mu.Lock()
	for id, cancel := range p.streams {
		cancel()
		delete(p.streams, id)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// Stage defines a concurrency level and duration for RunStages.
type Stage struct {
	Concurrency  int
	Duration     time.Duration
	Rampup       time.Duration // stagger new stream starts over this duration
	MaxRequests  int           // stop after this many requests (0 = unlimited)
	Barrier      bool          // sync point — pool stays alive (unless BarrierDrain), onBarrier fires
	BarrierDrain bool          // stop pool before sync, fresh pool after
}

// RunStages runs multiple stages with a shared goroutine pool, dynamically
// resizing concurrency between stages without tearing down in-flight requests.
// The onStage callback is called before each stage starts (for logging/metrics).
// The onBarrier callback is called at barrier sync points and should block until
// the barrier releases. The pool stays alive through non-drain barriers.
func (g *Generator) RunStages(ctx context.Context, stages []Stage, onStage func(index, concurrency int), onBarrier func(index int)) {
	c := client.New(g.Target)
	pool := newStreamPool(g, c)

	for i, stage := range stages {
		if ctx.Err() != nil {
			break
		}
		if stage.Barrier {
			if stage.BarrierDrain {
				pool.Stop()
				g.recordWG.Wait()
			}
			if onBarrier != nil {
				onBarrier(i)
			}
			if stage.BarrierDrain {
				pool = newStreamPool(g, c)
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
		pool.Resize(ctx, stage.Concurrency, stage.Rampup)

		if stage.MaxRequests > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(stage.Duration):
				slog.Warn("Stage timed out before all requests completed",
					"completed", state.count.Load(),
					"target", stage.MaxRequests)
			case <-state.done:
				drainDone := make(chan struct{})
				go func() { pool.Wait(); close(drainDone) }()
				select {
				case <-drainDone:
				case <-ctx.Done():
					pool.Stop()
				case <-time.After(2 * time.Minute):
					slog.Warn("Timed out waiting for in-flight requests to drain, cancelling",
						"completed", state.count.Load(),
						"target", stage.MaxRequests)
					pool.Stop()
				}
			}
		} else {
			select {
			case <-ctx.Done():
			case <-time.After(stage.Duration):
			}
		}
	}

	pool.Stop()
	g.recordWG.Wait()
}

func (g *Generator) Run(ctx context.Context) (*recorder.Timestamps, error) {
	c := client.New(g.Target)

	ctx, cancel := context.WithTimeout(ctx, g.Duration)
	defer cancel()

	startTime := time.Now()
	rampupEnd := startTime.Add(g.Rampup)

	switch g.Mode {
	case ModeConcurrent, "":
		g.runConcurrent(ctx, c)
	case ModeConstant:
		g.runRateBasedConstant(ctx, c, startTime)
	case ModePoisson:
		g.runRateBasedPoisson(ctx, c, startTime)
	default:
		return nil, fmt.Errorf("unknown mode: %s", g.Mode)
	}

	g.recordWG.Wait()
	endTime := time.Now()
	ts := &recorder.Timestamps{
		StartTime:     recorder.TimeToFloat(startTime),
		RampupEndTime: recorder.TimeToFloat(rampupEnd),
		EndTime:       recorder.TimeToFloat(endTime),
		RampupSeconds: g.Rampup.Seconds(),
		TotalSeconds:  endTime.Sub(startTime).Seconds(),
	}
	return ts, nil
}

// runConcurrent launches g.Concurrency streams, each sending requests back-to-back.
func (g *Generator) runConcurrent(ctx context.Context, c *client.Client) {
	var wg sync.WaitGroup
	for i := 0; i < g.Concurrency; i++ {
		wg.Add(1)
		delay := time.Duration(0)
		if g.Rampup > 0 && g.Concurrency > 1 {
			delay = g.Rampup * time.Duration(i) / time.Duration(g.Concurrency-1)
		}
		go func(streamID int, delay time.Duration) {
			defer wg.Done()
			g.runStream(ctx, c, streamID, delay)
		}(i, delay)
	}
	wg.Wait()
}

// runRateBasedConstant sends requests at evenly-spaced intervals.
func (g *Generator) runRateBasedConstant(ctx context.Context, c *client.Client, startTime time.Time) {
	g.runRateBased(ctx, c, startTime, false)
}

// runRateBasedPoisson sends requests with exponentially-distributed inter-arrival times.
func (g *Generator) runRateBasedPoisson(ctx context.Context, c *client.Client, startTime time.Time) {
	g.runRateBased(ctx, c, startTime, true)
}

func (g *Generator) runRateBased(ctx context.Context, c *client.Client, startTime time.Time, poisson bool) {
	var sem chan struct{}
	if g.MaxInFlight > 0 {
		sem = make(chan struct{}, g.MaxInFlight)
	}

	var wg sync.WaitGroup
	streamID := 0

	for {
		if ctx.Err() != nil {
			break
		}

		// Compute next arrival time
		elapsed := time.Since(startTime).Seconds()
		rate := g.rate(elapsed)
		if rate <= 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		var gap time.Duration
		if poisson {
			// Exponential inter-arrival time
			gap = time.Duration(float64(time.Second) * (-math.Log(1-mathrand.Float64()) / rate))
		} else {
			gap = time.Duration(float64(time.Second) / rate)
		}

		select {
		case <-ctx.Done():
			break
		case <-time.After(gap):
		}

		if ctx.Err() != nil {
			break
		}

		// Enforce max in-flight
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break
			}
			if ctx.Err() != nil {
				break
			}
		}

		conversation := g.Dataset.NextConversation()
		convID := fmt.Sprintf("w%d-c%d", streamID, streamID)
		sid := streamID
		streamID++

		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			g.runConversation(ctx, c, sid, convID, conversation)
		}()
	}

	wg.Wait()
}

// rate returns the effective request rate at a given elapsed time,
// accounting for linear rampup.
func (g *Generator) rate(elapsed float64) float64 {
	if g.Rampup.Seconds() <= 0 || elapsed >= g.Rampup.Seconds() {
		return g.Rate
	}
	// Linear ramp from 0 to target rate
	return g.Rate * (elapsed / g.Rampup.Seconds())
}

func (g *Generator) runStream(ctx context.Context, c *client.Client, streamID int, delay time.Duration) {
	if delay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	// Prefetch the next conversation while the current request is in-flight.
	// This overlaps dataset preparation with network I/O to minimize gaps.
	type pending struct {
		conv      dataset.Conversation
		convID    string
		exhausted bool
	}
	prefetch := make(chan pending, 1)
	nextID := 0

	fill := func() {
		if state := g.maxReqState.Load(); state != nil && state.limit > 0 {
			n := state.count.Add(1)
			if n > state.limit {
				state.once.Do(func() { close(state.done) })
				prefetch <- pending{exhausted: true}
				return
			}
		}
		conv := g.Dataset.NextConversation()
		id := fmt.Sprintf("w%d-c%d", streamID, nextID)
		nextID++
		prefetch <- pending{conv: conv, convID: id}
	}

	// Seed the prefetch buffer
	go fill()

	for {
		if ctx.Err() != nil {
			return
		}

		var p pending
		select {
		case p = <-prefetch:
		case <-ctx.Done():
			return
		}

		if p.exhausted {
			return
		}

		// Start prefetching the next conversation while this request runs
		go fill()

		g.runConversation(ctx, c, streamID, p.convID, p.conv)
	}
}

// cacheSalt returns the cache salt for a single request.
func (g *Generator) cacheSalt() string {
	if g.CacheSalt == nil {
		return ""
	}
	switch g.CacheSalt.Mode {
	case "random":
		var b [32]byte
		rand.Read(b[:])
		return base64.RawURLEncoding.EncodeToString(b[:])
	case "fixed":
		return g.CacheSalt.Value
	default:
		return ""
	}
}

// InFlight returns the current number of in-flight requests.
func (g *Generator) InFlight() int64 {
	return g.inFlight.Load()
}

// SetRecorder atomically swaps the recorder. In-flight recordResult goroutines
// may still write to the previous recorder; new ones will use the new recorder.
func (g *Generator) SetRecorder(r *recorder.Recorder) {
	g.recorderPtr.Store(r)
}

func (g *Generator) getRecorder() *recorder.Recorder {
	if r := g.recorderPtr.Load(); r != nil {
		return r
	}
	return g.Recorder
}

func (g *Generator) trackInFlight(delta int64) {
	n := g.inFlight.Add(delta)
	if g.Metrics != nil {
		g.Metrics.Concurrency.Set(float64(n))
	}
}

func (g *Generator) runCompletion(ctx context.Context, c *client.Client, streamID int, convID string, conv dataset.Conversation) {
	req := &client.CompletionRequest{
		Model:       g.Model,
		Prompt:      conv.Prompt,
		Stream:      true,
		MaxTokens:   conv.MaxTokens,
		Stop:        conv.Stop,
		Temperature: conv.Temperature,
		CacheSalt:   g.cacheSalt(),
	}

	g.trackInFlight(1)
	result := c.CompletionStream(ctx, req)
	g.trackInFlight(-1)

	// Don't record requests cancelled by shutdown
	if ctx.Err() != nil && result.Err == nil && result.FinishReason == "" {
		return
	}

	g.recordWG.Add(1)
	go func() {
		defer g.recordWG.Done()
		g.recordResult(result, streamID, convID, 0, conv)
	}()
}

// recordResult handles eval, metrics, and recording for a completed request.
func (g *Generator) recordResult(result *client.Result, streamID int, convID string, turn int, conv dataset.Conversation) {
	rec := &recorder.Record{
		RequestID:      fmt.Sprintf("%s-t%d", convID, turn),
		StreamID:       streamID,
		ConversationID: convID,
		Turn:           turn,
		StartTime:      recorder.TimeToFloat(result.RequestStart),
		EndTime:        recorder.TimeToFloat(result.EndTime),
		TotalLatencyMs: result.TotalLatency().Seconds() * 1000,
		OutputTokens:   result.OutputTokens(),
	}

	if result.Err != nil {
		rec.Status = "error"
		rec.Error = result.Err.Error()
		if g.Metrics != nil {
			g.Metrics.RequestsTotal.WithLabelValues("error").Inc()
		}
		if err := g.getRecorder().Write(rec); err != nil {
			slog.Error("Recorder write error", "error", err)
		}
		return
	}

	rec.Status = "ok"
	rec.FinishReason = result.FinishReason
	rec.TTFT = result.TTFT().Seconds() * 1000

	itls := result.ITLs()
	rec.ITLs = make([]float64, len(itls))
	for i, d := range itls {
		rec.ITLs[i] = d.Seconds() * 1000
	}

	if result.Usage != nil {
		rec.PromptTokens = result.Usage.PromptTokens
		rec.OutputTokens = result.Usage.CompletionTokens
	}

	// Evaluate response if expected answer is set
	if conv.ExpectedAnswer != "" {
		var extracted string
		var correct bool
		if eval.IsMCAnswer(conv.ExpectedAnswer) {
			extracted = eval.ExtractMCAnswer(result.Content)
			correct = eval.CheckMCCorrect(conv.ExpectedAnswer, extracted)
		} else {
			extracted = eval.ExtractAnswer(result.Content)
			correct = eval.CheckCorrect(conv.ExpectedAnswer, extracted)
		}
		rec.EvalExpected = conv.ExpectedAnswer
		rec.EvalExtracted = extracted
		rec.EvalCorrect = &correct
		if !correct {
			snippet := result.Content
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			slog.Debug("Eval miss",
				"conv", convID,
				"expected", conv.ExpectedAnswer,
				"extracted", extracted,
				"response", snippet)
		}
		if g.Metrics != nil {
			g.Metrics.RecordEval(correct)
		}

		// Periodic eval summary
		total := g.evalCount.Add(1)
		if correct {
			g.evalCorrect.Add(1)
		}
		if total%100 == 0 {
			c := g.evalCorrect.Load()
			slog.Info("Eval progress",
				"total", total,
				"correct", c,
				"accuracy", fmt.Sprintf("%.1f%%", float64(c)/float64(total)*100),
				"finish_reason", rec.FinishReason)
		}
	}

	// Emit Prometheus metrics
	if g.Metrics != nil {
		g.Metrics.RequestsTotal.WithLabelValues("ok").Inc()
		if rec.FinishReason != "" {
			g.Metrics.FinishReasons.WithLabelValues(rec.FinishReason).Inc()
		}
		if rec.TTFT > 0 {
			g.Metrics.TTFTSeconds.Observe(rec.TTFT / 1000)
		}
		for _, itl := range rec.ITLs {
			g.Metrics.ITLSeconds.Observe(itl / 1000)
		}
		g.Metrics.E2ESeconds.Observe(rec.TotalLatencyMs / 1000)
		g.Metrics.OutputTokens.Observe(float64(rec.OutputTokens))
		g.Metrics.PromptTokens.Observe(float64(rec.PromptTokens))
	}

	if err := g.getRecorder().Write(rec); err != nil {
		slog.Error("Recorder write error", "error", err)
	}
}

func (g *Generator) runConversation(ctx context.Context, c *client.Client, streamID int, convID string, conv dataset.Conversation) {
	// Completions API path (e.g., GSM8K with few-shot)
	if conv.Prompt != "" {
		g.runCompletion(ctx, c, streamID, convID, conv)
		return
	}

	// Build history dynamically so real model responses become the prefix
	// for subsequent turns. This enables KV cache reuse via prefix caching
	// (e.g., vLLM's --enable-prefix-caching). The dataset pre-builds turns
	// with synthetic assistant placeholders; we extract only the new user
	// message from each turn and substitute real responses.
	var history []client.Message

	for turnIdx, prebuilt := range conv.Turns {
		if ctx.Err() != nil {
			return
		}

		// The last message in each pre-built turn is the new user message.
		userMsg := prebuilt[len(prebuilt)-1]
		history = append(history, userMsg)

		messages := make([]client.Message, len(history))
		copy(messages, history)

		req := &client.Request{
			Model:     g.Model,
			Messages:  messages,
			Stream:    true,
			MaxTokens: conv.MaxTokens,
			CacheSalt: g.cacheSalt(),
		}

		g.trackInFlight(1)
		result := c.ChatStream(ctx, req)
		g.trackInFlight(-1)

		// Don't record requests cancelled by shutdown
		if ctx.Err() != nil && result.Err == nil && result.FinishReason == "" {
			return
		}

		g.recordWG.Add(1)
		go func(turn int) {
			defer g.recordWG.Done()
			g.recordResult(result, streamID, convID, turn, conv)
		}(turnIdx)

		if result.Err != nil {
			return
		}

		// Append the real model response so subsequent turns reuse the
		// server's KV cache instead of sending synthetic placeholders.
		history = append(history, client.Message{
			Role:    "assistant",
			Content: result.Content,
		})
	}
}
