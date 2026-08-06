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

JSON and YAML use the same typed workload schema, defaults, duration parsing,
and validation. YAML is convenient for checked-in profiles and for systems
such as llm-d-benchmark that apply treatment overrides and emit YAML:

```yaml
warmup:
  duration: 30s
  stagger: true
stages:
  - concurrency: 8
    duration: 2m
  - concurrency: 96
    duration: 3m
workload:
  type: faker
  name: llm-d-concurrency-sweep
  isl: 1024
  osl: 512
  turns: 2
```

```bash
nyann-bench generate --target http://localhost:8000/v1 \
  --config deploy/examples/concurrent-faker.yaml
```

YAML files use `.yaml` or `.yml`. Files with another extension are detected as
JSON when their first non-space character is `{` or `[`, and as YAML otherwise;
this covers rendered names such as `.yaml.in`. Inline JSON is detected the same
way. Prefix inline YAML with the `---` document marker so it cannot be mistaken
for a file path. YAML parsing is strict: unknown or duplicate fields are errors.

For programmable scenarios, use the Pythonic
[Starlark](https://github.com/google/starlark-go) DSL:

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

For KV-cache working-set experiments, use `mode="conversation_pool"` to decouple HOT active requests from the total number of active conversations. In this mode, `concurrency` controls the number of actively running inference requests, while `conversation_pool_size` controls how many conversations are kept in rotation. Completed turns resume the least recently used ready conversation; completed conversations are replaced with fresh ones.

Conversation-pool entries are materialized lazily when first scheduled, so large pools do not block stage startup on remote tokenization. For reasoning models, nyann-bench keeps the visible answer and reasoning output separate for evaluation, but replays both in conversation-pool history. This makes subsequent prompts reflect the full generated workload and KV-cache footprint. Ordinary multi-turn mode replays only the visible answer. When vLLM uses a reasoning parser, parser-only boundary tokens may not be present in the replayed text, so the reconstructed prefix is not guaranteed to be token-for-token identical to the original generation.

```python
scenario(
    stages = [
        stage("5m", mode="conversation_pool", concurrency=128, conversation_pool_size=n)
        for n in [128, 512, 2048, 8192]
    ],
    workload = workload("faker", isl=1024, subsequent_isl=64, osl=256, turns=3),
)
```

### Synchronized multi-pod start with automatic load division

When running across multiple pods, `--workers N` (where N > 1) enables barrier synchronization and automatically divides load across workers. Concurrency and rate values in config files always express the **total** desired load — each worker gets its fair share via integer division, with remainder distributed to lower-indexed workers (e.g. `concurrency=10, workers=3` → 4, 3, 3).

```bash
# Run with 4 workers — each gets 1/4 of the configured concurrency and rate
nyann-bench generate --target http://vllm:8000/v1 --config scenario.star --workers 4 --worker-id 0
```

All pods negotiate a common start time via an HTTP barrier protocol — pod-0 (leader) runs the barrier server, workers discover it via `BARRIER_ADDR` (set automatically in the Job manifest to the leader pod's DNS name). An implicit barrier is inserted before the first measured stage. Barriers are first-class in the Starlark DSL:

```python
scenario(
    stages=[
        stage("2m", concurrency=16, warmup=True),
        # implicit barrier fires here — all workers sync before measured stages
        stage("5m", concurrency=64),
        barrier(drain=True),  # explicit: drain pool before workload switch
        stage("5m", concurrency=64, workload=other),
    ],
)
```

With `--workers auto`, the worker count is `ceil(max_concurrency / 1024)`, sized for the **peak** stage. In staircase configs where concurrency ramps up across stages (e.g. `[4, 64, 512, 2048]`), early low-concurrency stages will have some workers with very few or zero streams. Use an explicit `--workers N` if you need tighter control.

With `--workers 1` (the default), no barrier sync or load division occurs.

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

Or with a YAML or Starlark config file:

```bash
./nyann-bench generate --target http://localhost:8000/v1 --config deploy/examples/concurrent-faker.yaml
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
| `conversation_pool` | Fixed HOT active requests over a configurable active conversation working set |
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

This creates a ConfigMap with your config and launches an Indexed Job with 8 pods. Each pod auto-detects its worker ID from `JOB_COMPLETION_INDEX` and the barrier server address from `BARRIER_ADDR`. The manifest passes `--workers N` so barrier sync and load division are enabled automatically.

For OpenShift, select the platform explicitly. This removes the privileged
network-tuning init container, applies restricted-compatible security
contexts, and creates a PodMonitor:

```bash
nyann-bench eval gsm8k \
  --target http://model-gateway/v1 \
  --gsm8k-path /mnt/shared/data/gsm8k_test.jsonl \
  --gsm8k-train-path /mnt/shared/data/gsm8k_train.jsonl \
  --output-dir /mnt/shared/results/run-001 \
  --kube --kube.context pirate --kube.namespace tms \
  --kube.platform openshift --kube.arch amd64 \
  --kube.config '{"volumes":[{"pvc":"shared-cache","mountPath":"/mnt/shared"}]}'
```

## In-cluster run API

`nyann-bench-api` is a small control plane for agents and other automation. It
keeps Kubernetes credentials inside the cluster and accepts nyann-bench's
native command vector; it does not define another workload or evaluation
schema. The command must target an existing service URL, normally one created
by Manifesto.

Create a strong bearer token and an explicit policy. Empty host/PVC lists deny
all corresponding access. The DNS suffix should be scoped to the namespace(s)
where Manifesto creates model Services:

```bash
kubectl -n benchmarks create secret generic nyann-bench-api-auth \
  --from-literal=token='REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS'
kubectl -n benchmarks create configmap nyann-bench-api-policy \
  --from-literal=allowed-target-hosts='' \
  --from-literal=allowed-target-suffixes='.models.svc' \
  --from-literal=allowed-pvcs='benchmark-results,benchmark-datasets'
```

Replace every `REPLACE_WITH_IMAGE_DIGEST` in `deploy/api.yaml` with the same
reviewed 64-character sha256 digest, then install the namespace-scoped API and
RBAC:

```bash
kubectl -n benchmarks apply -f deploy/api.yaml
```

The API and every benchmark run are CPU-only. Runs are ordinary Indexed Jobs:
they have no GPU requests, Kueue queue labels, or suspended admission state.
Manifesto remains responsible for deploying GPU-serving workloads through
Kueue.

Create a native generate run against a Manifesto service:

```bash
curl -sS http://nyann-bench-api.benchmarks.svc:8080/v1/runs \
  -H 'authorization: Bearer REPLACE_WITH_TOKEN' \
  -H 'content-type: application/json' \
  -d '{
    "name": "llama-smoke",
    "command": [
      "generate",
      "--target", "http://llama-decode.models.svc:8000/v1",
      "--config", "{\"load\":{\"concurrency\":32,\"duration\":\"2m\"}}"
    ],
    "workers": 2,
    "cpu": "4",
    "memory": "8Gi",
    "active_deadline_seconds": 3600,
    "ttl_seconds_after_finished": 86400,
    "results": {
      "pvc": "benchmark-results",
      "mount_path": "/results",
      "subdir": "nightly"
    }
  }'
```

`command` is passed as an argument array, so inline JSON does not undergo shell
expansion. For the secure API path, `--config` must be inline JSON; file and
Starlark configs are rejected because the control plane cannot inspect them for
per-stage target overrides. The API also rejects Prometheus and auxiliary URL
overrides. It owns `--workers`, `--worker-id`, `--metrics`,
`--output-dir`, and all `--kube*` flags. Durable runs report a `pvc://` URI and
write nyann-bench's existing `requests_N.jsonl` and `timestamps_N.json` files;
uploading those files to an artifact store is intentionally left for a later
integration.

An eval uses the same endpoint and the native eval command:

```bash
curl -sS http://nyann-bench-api.benchmarks.svc:8080/v1/runs \
  -H 'authorization: Bearer REPLACE_WITH_TOKEN' \
  -H 'content-type: application/json' \
  -d '{
    "command": [
      "eval", "gsm8k",
      "--target", "http://llama-decode.models.svc:8000/v1",
      "--gsm8k-path", "/datasets/gsm8k_test.jsonl",
      "--gsm8k-train-path", "/datasets/gsm8k_train.jsonl"
    ],
    "mounts": [
      {"pvc": "benchmark-datasets", "mount_path": "/datasets"}
    ]
  }'
```

Inspect and control runs:

```bash
curl -sS -H 'authorization: Bearer REPLACE_WITH_TOKEN' http://nyann-bench-api.benchmarks.svc:8080/v1/runs
curl -sS -H 'authorization: Bearer REPLACE_WITH_TOKEN' http://nyann-bench-api.benchmarks.svc:8080/v1/runs/llama-smoke
curl -sS -H 'authorization: Bearer REPLACE_WITH_TOKEN' 'http://nyann-bench-api.benchmarks.svc:8080/v1/runs/llama-smoke/logs?tail_lines=500'
curl -i -X DELETE -H 'authorization: Bearer REPLACE_WITH_TOKEN' http://nyann-bench-api.benchmarks.svc:8080/v1/runs/llama-smoke
```

The server enforces operator-configured ceilings for workers, per-worker CPU
and memory, active runtime, and completed-Job retention. The defaults in the
manifest are 16 workers, 16 CPU, 64 GiB, six hours maximum runtime, and seven
days maximum retention. PVC access and Manifesto target hosts are explicit
allowlists; their secure default is deny-all. The runner image is
operator-managed and must be an immutable official digest.

The supplied RBAC can only manage Jobs, their headless Services, and pod logs
in its own namespace. Bearer authentication is required for every `/v1`
endpoint, but NetworkPolicy should still limit callers. Apply a namespace
`ResourceQuota` as defense in depth because aggregate namespace capacity and
other workload controllers are outside this API's scope, for example:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: nyann-bench-ceiling
spec:
  hard:
    requests.cpu: "64"
    requests.memory: 256Gi
    limits.cpu: "64"
    limits.memory: 256Gi
    count/jobs.batch: "32"
```

## Installation

```bash
go install github.com/neuralmagic/nyann-bench/cmd/nyann-bench@latest
```

Or pull the container:

```bash
docker pull ghcr.io/neuralmagic/nyann-bench:latest
```

## Container images

CI pushes multi-platform (`linux/amd64`, `linux/arm64`) images to GitHub Container Registry on every push to `main` and every pull request:

| Event | Tag | Example |
|-------|-----|---------|
| Push to `main` | `latest`, `sha-<commit>` | `ghcr.io/neuralmagic/nyann-bench:latest` |
| Pull request | `pr-<number>` | `ghcr.io/neuralmagic/nyann-bench:pr-47` |

To use a PR image for testing:

```bash
docker pull ghcr.io/neuralmagic/nyann-bench:pr-47
```

## Development

```bash
go test ./... -count=1     # all tests run against the mock server
just test                  # same, via Justfile
just smoke-test            # end-to-end: mock server + load generator
```
