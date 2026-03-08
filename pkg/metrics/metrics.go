package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics exposes Prometheus metrics for streaming eval.
type Metrics struct {
	RequestsTotal   *prometheus.CounterVec
	EvalTotal       prometheus.Counter
	EvalCorrect     prometheus.Counter
	EvalIncorrect   prometheus.Counter
	Accuracy        prometheus.Gauge
	Concurrency     prometheus.Gauge
	Stage           prometheus.Gauge
	TTFTSeconds     prometheus.Histogram
	ITLSeconds      prometheus.Histogram
	E2ESeconds      prometheus.Histogram
	OutputTokens    prometheus.Histogram
	PromptTokens    prometheus.Histogram

	correctCount float64
	totalCount   float64
}

// New creates metrics with a workload label identifying the eval/workload name.
// If workloadName is empty, no workload label is added.
// If enableEval is false, eval metrics (accuracy, correct, incorrect, total) are not registered.
func New(reg *prometheus.Registry, workloadName string, enableEval bool) *Metrics {
	var constLabels prometheus.Labels
	if workloadName != "" {
		constLabels = prometheus.Labels{"workload": workloadName}
	}

	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "nyann_requests_total",
			Help:        "Total requests by status",
			ConstLabels: constLabels,
		}, []string{"status"}),

		Concurrency: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "nyann_concurrency",
			Help:        "Current concurrency level",
			ConstLabels: constLabels,
		}),
		Stage: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "nyann_stage",
			Help:        "Current stage index (0-based)",
			ConstLabels: constLabels,
		}),

		TTFTSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "nyann_ttft_seconds",
			Help:        "Time to first token",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~160s
		}),
		ITLSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "nyann_itl_seconds",
			Help:        "Inter-token latency",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
		}),
		E2ESeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "nyann_e2e_seconds",
			Help:        "End-to-end request latency",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(0.1, 2, 15), // 100ms to ~1600s
		}),
		OutputTokens: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "nyann_output_tokens",
			Help:        "Output tokens per request",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(1, 2, 14), // 1 to ~8192
		}),
		PromptTokens: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        "nyann_prompt_tokens",
			Help:        "Prompt tokens per request",
			ConstLabels: constLabels,
			Buckets:     prometheus.ExponentialBuckets(1, 2, 16), // 1 to ~32768
		}),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.Concurrency, m.Stage,
		m.TTFTSeconds, m.ITLSeconds, m.E2ESeconds,
		m.OutputTokens, m.PromptTokens,
	)

	if enableEval {
		m.EvalTotal = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "nyann_eval_total",
			Help:        "Total evaluated responses",
			ConstLabels: constLabels,
		})
		m.EvalCorrect = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "nyann_eval_correct",
			Help:        "Correctly answered responses",
			ConstLabels: constLabels,
		})
		m.EvalIncorrect = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "nyann_eval_incorrect",
			Help:        "Incorrectly answered responses",
			ConstLabels: constLabels,
		})
		m.Accuracy = prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "nyann_eval_accuracy",
			Help:        "Running accuracy (correct / total evaluated)",
			ConstLabels: constLabels,
		})
		reg.MustRegister(m.EvalTotal, m.EvalCorrect, m.EvalIncorrect, m.Accuracy)
	}

	return m
}

// RecordEval updates eval counters and accuracy gauge.
// No-op if eval metrics were not enabled.
func (m *Metrics) RecordEval(correct bool) {
	if m.EvalTotal == nil {
		return
	}
	m.EvalTotal.Inc()
	m.totalCount++
	if correct {
		m.EvalCorrect.Inc()
		m.correctCount++
	} else {
		m.EvalIncorrect.Inc()
	}
	if m.totalCount > 0 {
		m.Accuracy.Set(m.correctCount / m.totalCount)
	}
}

// Handler returns an HTTP handler for the /metrics endpoint.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
