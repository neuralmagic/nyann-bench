# nyann-bench

High-performance LLM inference benchmarking tool. "Not Yet Another Neural Network... Benchmark."

## Build / Run

```bash
go build -o nyann-bench ./cmd/nyann-bench/
go test ./... -count=1
```

## Project structure

- `cmd/nyann-bench/` — CLI entry point (subcommands: generate, mock-server, analyze, corpus)
- `pkg/mockserver/` — OpenAI-compatible mock inference server for testing
- `pkg/client/` — Streaming OpenAI chat completions client with token-level timing
- `pkg/loadgen/` — Goroutine pool load generator with rampup and multi-turn support
- `pkg/dataset/` — Dataset interfaces and implementations (synthetic, faker, corpus, gsm8k)
- `pkg/eval/` — Answer extraction and correctness checking for streaming eval
- `pkg/metrics/` — Prometheus metrics (request latencies, eval accuracy)
- `pkg/recorder/` — Per-request JSONL recording and timestamps JSON output
- `pkg/barrier/` — HTTP barrier server/client for multi-pod synchronized start
- `pkg/config/` — JSON and Starlark config parsing (ScenarioConfig IR)

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
- **Barrier sync** — multi-pod synchronization via HTTP barrier. Pod-0 (leader) runs a barrier server; all pods negotiate a common start time before measured stages. Configured via `--sync '{"workers":N}'` CLI flag and `barrier()` in Starlark DSL.

## Deployment

Deployed via an **Indexed Job** on Kubernetes. Single-worker mode uses a Job with `completions: 1`.

```bash
just deploy my-bench http://vllm:8000/v1 config.json N_WORKERS=4
```

### Multi-pod synchronization

Use `--sync` to synchronize benchmark start across pods:

```bash
nyann-bench generate --config scenario.star --sync '{"workers":4,"timeout":"10m"}'
```

An implicit `barrier()` is inserted before the first measured stage. In Starlark, use explicit `barrier()` for additional sync points:

```python
scenario(
    stages=[
        stage("2m", concurrency=16, warmup=True),
        # implicit barrier fires here
        stage("5m", concurrency=64),
        barrier(drain=True),  # explicit: drain pool before workload switch
        stage("5m", concurrency=64, workload=other),
    ],
)
```

## Go module

`github.com/neuralmagic/nyann-bench`
