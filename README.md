# nyann-bench

**N**ot **Y**et **A**nother **N**eural **N**etwork **Bench**marking Tool.

A high-performance LLM inference benchmarking tool designed for Kubernetes-scale deployments.

## Why nyann-bench?

`nyann-bench` was ~~vibe-coded~~ created via agentic engineering in support of vLLM's GB200 NVL72 WideEP bring-up, in order to address a series of challenges we ran into at scale.

1. In order to sustain a high number of concurrent requests, a benchmarking tool needs to support scale-out and a high request rate at high concurrency.
2. Observability becomes more important at scale. Client-side benchmarking metrics make it easy to see what all benchmarking pods are doing at a glance.
3. Streaming evals helped us detect and debug numerical issues that would gradually degrade the accuracy of NVFP4 models over the lifetime of the server — rare events that would only happen at scale.
4. Tools like `vllm bench`, `guide-llm` or `lm-eval` that have heavy dependencies like PyTorch are too slow to update or deploy. `nyann-bench` is only 5MB compressed.

### Pretty Fast

At high concurrency, nyann-bench sustains up to **10x more requests per second** than Python-based alternatives. Go's goroutine model and tuned HTTP transport eliminate the client as the bottleneck, so you're measuring the server, not your benchmark harness.

| Concurrency | nyann-bench | guidellm | vllm bench |
|-------------|-------------|----------|------------|
| 1           | 28 req/s    | 28 req/s | 28 req/s   |
| 64          | 1,616 req/s | 1,341 req/s | 1,386 req/s |
| 256         | 7,221 req/s | 1,352 req/s | 2,083 req/s |
| 1024        | 15,065 req/s | 1,207 req/s | 2,120 req/s |
| 4096        | **17,889 req/s** | 1,306 req/s | 1,799 req/s |

<sup>Measured against the built-in mock server on a Linux x86_64 machine, 30s per data point. See [bench_compare/](bench_compare/) for methodology and reproduction steps.</sup>

### Kubernetes-native

The container image is **~5 MB** (single static binary on `scratch`) — no Python runtime, no pip dependencies, no conda environment. It deploys as a Kubernetes [Indexed Job](https://kubernetes.io/docs/concepts/workloads/controllers/job/#completion-mode) with a headless Service for horizontal scale-out across multiple pods, with built-in barrier synchronization so all pods start their measured stages at the exact same wall-clock time. Pod-level network tuning (expanded ephemeral port range, `TCP_TW_REUSE`) is built into the manifest.

### Streaming eval

Run GSM8K (or other evals) under load to see accuracy in real time via Prometheus. Watch your inference server's GSM8K score slowly fall as its KV cache gets poisoned with NaNs.

### Prometheus integration

Two-sided observability out of the box:

- **Client-side metrics** — each pod exposes a `/metrics` endpoint with histograms for TTFT, ITL, E2E latency, and token counts, ready for Prometheus scraping.
- **Server-side correlation** — per-stage timestamps make it easy to query your server's Prometheus for the exact window of each benchmark phase (see `just query-prometheus`).

### Flexible workload definition

Define benchmark scenarios using a Pythonic [Starlark](https://github.com/google/starlark-go) DSL:

```python
chat = workload("faker", isl=256, osl=512)
long = workload("corpus", corpus_path="/data/sharegpt.txt", isl=2048, osl=512)

scenario(
    stages = [
        stage("30s", concurrency=16, warmup=True),
        stage("5m",  concurrency=128, workload=chat),
        stage("5m",  concurrency=64,  workload=long),
    ],
)
```

Use variables, loops, and conditionals — it's a real language, not YAML:

```python
scenario(
    stages = [stage("2m", concurrency=c) for c in range(64, 513, 64)],
    workload = workload("synthetic", isl=512, osl=1024),
)
```

### Multi-turn conversations

Each goroutine stream can run multi-turn conversations, carrying real model responses forward into subsequent turns. This exercises server-side KV cache reuse (prefix caching) and produces realistic conversation-shaped traffic.

### Synchronized multi-pod start

When running across multiple pods, `--sync '{"workers":N}'` enables barrier synchronization. All pods negotiate a common start time via an HTTP barrier protocol — pod-0 (leader) runs the barrier server, workers discover it via `BARRIER_ADDR` (set automatically in the Job manifest to the leader pod's DNS name). Barriers are first-class in the Starlark DSL:

```python
scenario(
    stages=[
        stage("2m", concurrency=16, warmup=True),
        barrier(),                                  # implicit one added automatically
        stage("5m", concurrency=64),
        barrier(drain=True),                        # drain pool before workload switch
        stage("5m", concurrency=64, workload=other),
    ],
)
```

### Ramp-up and warmup

A configurable warmup phase brings the server to steady state before measurement begins, and ramp-up staggers stream starts to avoid synchronized request patterns that would otherwise create artificial load spikes.

## Quick start

```bash
# Build
go build -o nyann-bench ./cmd/nyann-bench/

# Start the mock server (for testing)
./nyann-bench mock-server

# Run a quick benchmark
./nyann-bench generate --target http://localhost:8000/v1 --config '{"load":{"concurrency":16,"duration":"30s"}}'
```

Or with a Starlark config file:

```bash
./nyann-bench generate --target http://localhost:8000/v1 --config scenario.star
```

## Subcommands

| Command | Description |
|---------|-------------|
| `generate` | Run a load generation benchmark against an LLM endpoint |
| `analyze` | Analyze benchmark results from JSONL recordings |
| `mock-server` | Start a mock OpenAI-compatible server for testing |
| `corpus` | Convert text sources (ShareGPT, files, directories) into a corpus file |

## Workload types

| Type | Description |
|------|-------------|
| `synthetic` | Random word padding with deterministic ISL/OSL control |
| `faker` | Diverse, realistic generated text (names, locations, phrases) |
| `corpus` | Sliding window over real text files (ShareGPT, custom corpora) |
| `gsm8k` | Grade School Math 8K with few-shot prompting and streaming eval |

All workload types support configurable ISL (input sequence length), OSL (output sequence length), multi-turn conversations, and per-turn ISL overrides via `subsequent_isl`.

## Load modes

| Mode | Description |
|------|-------------|
| `concurrent` | Fixed number of goroutine streams, each sending requests back-to-back |
| `constant` | Fixed request rate (req/s) with deterministic inter-arrival times |
| `poisson` | Fixed request rate with exponential inter-arrival times (realistic traffic) |

## Output

Each worker produces:

- **`requests_N.jsonl`** — one line per completed request with TTFT, per-token ITL array, token counts, latency, eval results, and finish reason.
- **`timestamps_N.json`** — start/end times for each stage, for Prometheus range queries.

Merging across workers: `cat requests_*.jsonl`.

## Kubernetes deployment

```bash
just deploy my-benchmark http://vllm-server:8000/v1 config.star 8
```

This creates a ConfigMap with your config and launches an Indexed Job with 8 pods. Each pod auto-detects its worker ID from `JOB_COMPLETION_INDEX` and the barrier server address from `BARRIER_ADDR`. Sync is enabled automatically via `--sync '{"workers":N}'` in the manifest.

## Installation

```bash
go install github.com/neuralmagic/nyann-bench/cmd/nyann-bench@latest
```

Or pull the container:

```bash
docker pull ghcr.io/neuralmagic/nyann-bench:latest
```

## Development

```bash
go test ./... -count=1     # all tests run against the mock server
just test                  # same, via Justfile
just smoke-test            # end-to-end: mock server + load generator
```
