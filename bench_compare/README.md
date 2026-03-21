# Benchmark Tool Comparison

Measures sustained request rate of nyann_poker vs guidellm vs vllm bench serve
at increasing concurrency levels, using the built-in mock server as the target.

The mock server has very fast token latencies (1ms TTFT, 500us ITL, 32 tokens)
so the bottleneck is the benchmarking client, not the server.

## Setup

```bash
# Build nyann_poker
go build -o nyann_poker ./cmd/nyann_poker/

# Install competitor tools (requires uv)
./bench_compare/setup.sh
```

## Run

```bash
./bench_compare/run.sh                     # default concurrency levels
./bench_compare/run.sh "1 16 256 4096"     # custom levels
```

## Results

Measured on a Linux x86_64 machine (H100 node). 30 seconds per tool per
concurrency level. `ulimit -n 65536`.

```
Tool           Conc      Req/s   Requests   Errors    RSS(MB)
--------------------------------------------------------------
nyann             1       28.3       849        0       28.9
guidellm          1       28.1       842        0      845.1
vllm              1       27.9      1016        0     1097.7

nyann             4      111.8      3353        0       34.5
guidellm          4      111.0      3329        0      868.5
vllm              4      110.7      3933        0      967.6

nyann            16      424.4     12729        0       48.0
guidellm         16      447.4     13422        0      985.0
vllm             16      431.7     14296        0     1005.8

nyann            64     1615.8     48473        0      106.2
guidellm         64     1341.0     40229        0     1456.8
vllm             64     1386.3     33974        0     1068.3

nyann           256     7220.6    216654        0      512.2
guidellm        256     1352.3     40570        0     1528.0
vllm            256     2083.3     43978        0     1109.5

nyann          1024    15064.5    452051        0      887.2
guidellm       1024     1206.7     36200        0     1383.0
vllm           1024     2119.9     50480        0     1145.7

nyann          4096    17889.4    541727        0     1302.1
guidellm       4096     1305.7     39171        0     1420.5
vllm           4096     1798.7     45652        0     1195.5
```

At low concurrency (1-16) all three tools are equivalent — the mock server is
the bottleneck. At c=64+, nyann_poker pulls ahead due to Go's goroutine model
and tuned HTTP transport. By c=4096:

- **nyann_poker: 17,889 req/s** — 13.7x guidellm, 9.9x vllm bench
- **guidellm plateaus ~1,300 req/s** regardless of concurrency (worker pool saturation)
- **vllm bench plateaus ~2,100 req/s** (single asyncio event loop)
- nyann_poker uses **29 MB at c=1** vs ~850-1100 MB baseline for the Python tools
