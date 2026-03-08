# nyann_poker

High-performance LLM inference benchmarking tool. "Not Yet Another Neural Network... Poker."

## Build / Run

```bash
go build -o nyann_poker ./cmd/bench/
go test ./... -count=1
```

## Project structure

- `cmd/nyann_poker/` — CLI entry point (subcommands: generate, mock-server, analyze, corpus)
- `pkg/mockserver/` — OpenAI-compatible mock inference server for testing
- `pkg/client/` — Streaming OpenAI chat completions client with token-level timing
- `pkg/loadgen/` — Goroutine pool load generator with rampup and multi-turn support
- `pkg/dataset/` — Dataset interfaces and implementations (synthetic, faker, corpus, gsm8k)
- `pkg/eval/` — Answer extraction and correctness checking for streaming eval
- `pkg/metrics/` — Prometheus metrics (request latencies, eval accuracy)
- `pkg/recorder/` — Per-request JSONL recording and timestamps JSON output

## Testing

All tests run against the mock server — no external dependencies needed.

```bash
go test ./... -v
```

## Architecture

- **Load generator** — each goroutine is a "stream" that sends a request, waits for completion, sends the next. Multi-turn conversations carry context forward across turns.
- **Client-side recording** — JSONL per worker. One line per completed request with timestamps, TTFT, per-token ITL array, token counts, status. Merging across workers = cat + sort.
- **Timestamps** — JSON file per worker with start_time, rampup_end_time, end_time. Used to query Prometheus for server-side metrics.
- **Mock server** — configurable TTFT, ITL, and output token count. Serves streaming SSE responses on `/v1/chat/completions`.

## Go module

`github.com/neuralmagic/nyann_poker`
