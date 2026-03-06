# bench — Roadmap

A high-performance, GPU-rich benchmarking platform for large-scale LLM inference.

## v0 — "Unblock GB200 WideEP benchmarking" (now)

Get the core load generator working so we can run Pareto sweeps on WideEP DeepSeek-R1.

- [ ] **Go project scaffolding** — module, CLI (cobra), Dockerfile (multi-stage → scratch)
- [ ] **Load generator core** — goroutine pool, staggered rampup, configurable concurrency, duration-based runs
- [ ] **OpenAI streaming client** — POST /v1/chat/completions with SSE parsing, token-level timing
- [ ] **Client-side recording** — per-request JSONL (timestamps, TTFT, per-token ITL, token counts, status). One file per worker. Lock-free channel-based writer
- [ ] **Datasets** — synthetic (configurable ISL/OSL with random token fill) + ShareGPT loader
- [ ] **Timestamps output** — JSON file with start_time, rampup_end_time, end_time per worker
- [ ] **Analysis CLI** — merge JSONL across workers, query Prometheus for server-side metrics, compute TPSU/TPSG/percentiles, output JSON + CSV
- [ ] **Pareto visualization** — generate TPSU vs TPSG Pareto curves (SVG/PNG or interactive HTML)
- [ ] **K8s manifests** — multi-container pod Job template (N load generator containers + shared volume for results)
- [ ] **Justfile recipes** — build, push image, run benchmark, collect results, analyze

### Non-goals for v0
- No multi-turn, no multimodal, no validation
- No GPU acceleration
- No auto steady-state detection (user picks warmup buffer)

---

## v1 — "Real workloads"

Support the workload diversity needed for production benchmarking.

- [ ] **Scenario system** — YAML configs that define dataset + load profile + validation. Scenarios are the unit of benchmarking
- [ ] **Multi-turn chat** — scenario defines conversation structure: turns, context carry-forward, think time between turns
- [ ] **Mixed workloads** — weighted distribution of request types within a single benchmark (e.g., 70% short chat, 20% long summarization, 10% code generation). Convex combination of other workloads.
- [ ] **Multimodal inputs** — image payloads (base64 or URL) and audio payloads in chat completion requests. Dataset loaders for vision benchmarks
- [ ] **Basic output validation** — regex/substring matching, JSON schema validation, token count bounds. Per-request pass/fail in JSONL
- [ ] **Auto steady-state detection** — watch Prometheus metrics (throughput CV, KV cache derivative) and automatically determine measurement window
- [ ] **Live dashboard** — real-time terminal UI showing throughput, latency percentiles, KV cache util during the run

---

## v2 — "GPU-accelerated eval"

Build our own fast lm-eval. Use client-side GPUs for preprocessing and validation.

- [ ] **GPU-accelerated validation** — run lightweight models on client GPUs to score outputs (coherence, correctness, safety). CUDA container image variant (`bench-gpu`)
- [ ] **Omni-modal I/O** — audio input (speech-to-text benchmarks), video input (frame extraction + encoding), audio/visual output validation
- [ ] **GPU-accelerated preprocessing** — encode images/audio/video on client GPUs before sending requests. Critical for high-throughput multimodal benchmarks
- [ ] **Eval framework** — pluggable eval metrics (BLEU, ROUGE, model-graded, custom). Per-request and aggregate scores. Comparison across runs
- [ ] **Reference answer datasets** — datasets with ground truth for eval. Support for few-shot prompting in eval

### Image size strategy
- Base image: `bench` on `scratch` (~10MB) for pure load generation
- GPU image: `bench-gpu` on slim CUDA runtime (~500MB) for validation/preprocessing
- Sidecar pattern: load generator container + GPU eval container in same pod, communicating via shared volume or unix socket

---

## v3 — "Platform"

Full benchmarking platform with historical tracking, CI integration, regression detection.

- [ ] **Results database** — store benchmark results (client-side + Prometheus snapshots) in a persistent store. Compare across runs, configs, dates
- [ ] **Regression detection** — nightly benchmarks with automatic alerting on performance regressions
- [ ] **CI integration** — GitHub Actions / Argo Workflows integration for benchmark-on-PR
- [ ] **Web dashboard** — interactive visualization, historical trends, Pareto curve explorer
- [ ] **Distributed coordination** — leader election for multi-pod benchmarks, synchronized start/stop across pods
- [ ] **Adaptive load** — closed-loop controller that adjusts concurrency to find saturation point automatically

## More ideas from Tyler:
 - Auto detection of steady state beginning and end so we can choose our benchmarking range (end of ramp-up, beginning of wind-down).
 - **t-digest for distributed percentile computation** — Each worker maintains a streaming t-digest (`github.com/caio/go-tdigest`) for ITL and TTFT during the run. At completion, emit serialized t-digests in the summary JSON. The analyze command merges t-digests across workers to compute exact-ish percentiles without shipping raw values. No pre-defined histogram buckets needed. Solves: (1) you can't merge percentiles across workers, (2) fixed histogram buckets lose resolution, (3) shipping millions of raw ITL floats through kubectl logs is impractical.

---

## Design principles

1. **Own your metrics** — don't trust the inference server's self-reported metrics. Collect client-side AND server-side (Prometheus) independently. Cross-validate.
2. **Fast startup** — single static binary, scratch container image, no install step. Pod startup = image pull + exec.
3. **Easy stitching** — JSONL per worker. Merging = cat + sort. No complex aggregation protocols between workers.
4. **Scenarios as config** — workloads are defined declaratively. The binary is general-purpose; scenarios encode the specifics.
5. **GPU-rich mentality** — when you have GPUs available on the client, use them. Preprocessing, validation, and eval should be acceleratable.
6. **Prometheus-native** — server-side metrics come from Prometheus. The tool knows how to query it, compute percentiles from histograms, and correlate with client-side data.
