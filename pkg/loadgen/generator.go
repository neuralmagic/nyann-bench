package loadgen

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/neuralmagic/nyann_poker/pkg/client"
	"github.com/neuralmagic/nyann_poker/pkg/dataset"
	"github.com/neuralmagic/nyann_poker/pkg/eval"
	"github.com/neuralmagic/nyann_poker/pkg/metrics"
	"github.com/neuralmagic/nyann_poker/pkg/recorder"
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
	Metrics     *metrics.Metrics // Optional Prometheus metrics (nil = disabled)
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
			gap = time.Duration(float64(time.Second) * (-math.Log(1-rand.Float64()) / rate))
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

	convID := 0
	for {
		if ctx.Err() != nil {
			return
		}

		conversation := g.Dataset.NextConversation()
		conversationID := fmt.Sprintf("w%d-c%d", streamID, convID)
		convID++

		g.runConversation(ctx, c, streamID, conversationID, conversation)
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
	}

	result := c.CompletionStream(ctx, req)

	rec := &recorder.Record{
		RequestID:      fmt.Sprintf("%s-t0", convID),
		StreamID:       streamID,
		ConversationID: convID,
		Turn:           0,
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
		if err := g.Recorder.Write(rec); err != nil {
			slog.Error("Recorder write error", "error", err)
		}
		return
	}

	rec.Status = "ok"
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
		extracted := eval.ExtractAnswer(result.Content)
		correct := eval.CheckCorrect(conv.ExpectedAnswer, extracted)
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
	}

	// Emit Prometheus metrics
	if g.Metrics != nil {
		g.Metrics.RequestsTotal.WithLabelValues("ok").Inc()
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

	if err := g.Recorder.Write(rec); err != nil {
		slog.Error("Recorder write error", "error", err)
	}
}

func (g *Generator) runConversation(ctx context.Context, c *client.Client, streamID int, convID string, conv dataset.Conversation) {
	// Completions API path (e.g., GSM8K with few-shot)
	if conv.Prompt != "" {
		g.runCompletion(ctx, c, streamID, convID, conv)
		return
	}

	for turnIdx, messages := range conv.Turns {
		if ctx.Err() != nil {
			return
		}

		req := &client.Request{
			Model:     g.Model,
			Messages:  messages,
			Stream:    true,
			MaxTokens: conv.MaxTokens,
		}

		result := c.ChatStream(ctx, req)

		rec := &recorder.Record{
			RequestID:      fmt.Sprintf("%s-t%d", convID, turnIdx),
			StreamID:       streamID,
			ConversationID: convID,
			Turn:           turnIdx,
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
			if err := g.Recorder.Write(rec); err != nil {
				slog.Error("Recorder write error", "error", err)
			}
			return
		}

		rec.Status = "ok"
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
			extracted := eval.ExtractAnswer(result.Content)
			correct := eval.CheckCorrect(conv.ExpectedAnswer, extracted)
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
		}

		// Emit Prometheus metrics
		if g.Metrics != nil {
			g.Metrics.RequestsTotal.WithLabelValues("ok").Inc()
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

		if err := g.Recorder.Write(rec); err != nil {
			slog.Error("Recorder write error", "error", err)
		}
	}
}
