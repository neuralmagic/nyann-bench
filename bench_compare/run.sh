#!/usr/bin/env bash
# Compare sustained request rate of nyann_poker, guidellm, and vllm bench
# at increasing concurrency levels against the mock server.
#
# Usage:
#   ./bench_compare/run.sh                       # default: 1 4 16 64 256 1024 4096
#   ./bench_compare/run.sh "1 16 256 4096"       # custom levels
#
# Prerequisites:
#   1. go build -o nyann_poker ./cmd/nyann_poker/
#   2. ./bench_compare/setup.sh
set -euo pipefail

ulimit -n 65536 2>/dev/null || true

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
NYANN="$PROJECT_DIR/nyann_poker"
GUIDELLM="$SCRIPT_DIR/venvs/guidellm/bin/guidellm"
VLLM_BIN="$SCRIPT_DIR/venvs/vllm/bin/vllm"

MOCK_PORT=18080
MOCK_URL="http://localhost:${MOCK_PORT}"
MOCK_TTFT="1ms"
MOCK_ITL="500us"
MOCK_TOKENS=32

RESULTS_DIR="$SCRIPT_DIR/results/$(date +%Y%m%d_%H%M%S)"
mkdir -p "$RESULTS_DIR"

CONCURRENCY_LEVELS="${1:-1 4 16 64 256 1024 4096}"
DURATION_SECS=30
TIMEOUT_SECS=$((DURATION_SECS + 120))

cleanup() {
    kill "$MOCK_PID" 2>/dev/null || true
    wait "$MOCK_PID" 2>/dev/null || true
}
trap cleanup EXIT

# --- Mock server ---
"$NYANN" mock-server \
    --addr ":${MOCK_PORT}" \
    --ttft "$MOCK_TTFT" \
    --itl "$MOCK_ITL" \
    --output-tokens "$MOCK_TOKENS" \
    --model mock-model \
    > "$RESULTS_DIR/mock_server.log" 2>&1 &
MOCK_PID=$!
sleep 1
curl -sf "${MOCK_URL}/health" > /dev/null || { echo "Mock server failed to start"; exit 1; }
echo "Mock server ready (pid=$MOCK_PID)"

echo ""
echo "================================================================"
echo "  Sustained req/s benchmark"
echo "  Server: ttft=${MOCK_TTFT} itl=${MOCK_ITL} tokens=${MOCK_TOKENS}"
echo "  Concurrency: ${CONCURRENCY_LEVELS}"
echo "  Duration: ~${DURATION_SECS}s per tool per level"
echo "  Results: ${RESULTS_DIR}"
echo "================================================================"

# --- Measurement wrapper ---
# Runs a command with a timeout, samples peak RSS from /proc, writes
# resource usage to <label>.resources.json. Tool stdout/stderr go to
# <label>.stdout / <label>.stderr.
cat > "$RESULTS_DIR/measure.py" << 'PYEOF'
import resource, sys, time, subprocess, json, threading, os

timeout = int(sys.argv[1])
outfile = sys.argv[2]
cmd = sys.argv[3:]
peak_rss_kb = 0

proc = subprocess.Popen(cmd, stdout=open(outfile + ".stdout", "w"),
                             stderr=open(outfile + ".stderr", "w"))

def sample_rss():
    global peak_rss_kb
    while proc.poll() is None:
        try:
            with open(f"/proc/{proc.pid}/status") as f:
                for line in f:
                    if line.startswith("VmRSS:"):
                        peak_rss_kb = max(peak_rss_kb, int(line.split()[1]))
                        break
        except (FileNotFoundError, ProcessLookupError, OSError):
            pass
        time.sleep(0.5)

t0 = time.monotonic()
threading.Thread(target=sample_rss, daemon=True).start()

try:
    proc.wait(timeout=timeout)
except subprocess.TimeoutExpired:
    proc.kill()
    proc.wait()

elapsed = time.monotonic() - t0
ru = resource.getrusage(resource.RUSAGE_CHILDREN)
rss_kb = peak_rss_kb if peak_rss_kb > 0 else ru.ru_maxrss

with open(outfile + ".resources.json", "w") as f:
    json.dump({"wall_s": round(elapsed, 2), "user_s": round(ru.ru_utime, 2),
               "sys_s": round(ru.ru_stime, 2), "max_rss_kb": rss_kb,
               "exit_code": proc.returncode, "timed_out": proc.returncode == -9}, f)

rss_mb = rss_kb / 1024
status = "TIMEOUT" if proc.returncode == -9 else f"exit={proc.returncode}"
print(f"  -> {elapsed:.1f}s wall, {rss_mb:.1f}MB RSS, {status}")
PYEOF

run_tool() {
    local label="$1"; shift
    python3 "$RESULTS_DIR/measure.py" "$TIMEOUT_SECS" "$RESULTS_DIR/${label}" "$@"
}

# --- vllm bench calibration ---
# vllm bench is count-based, not duration-based. Run a short calibration at
# each concurrency level to figure out the right --num-prompts.
calibrate_vllm() {
    local conc=$1
    local calib_dir="$RESULTS_DIR/calibration_c${conc}"
    mkdir -p "$calib_dir"
    "$VLLM_BIN" bench serve \
        --backend openai --base-url "${MOCK_URL}" --model mock-model \
        --endpoint /v1/chat/completions --dataset-name random \
        --input-len 64 --output-len "$MOCK_TOKENS" \
        --num-prompts 200 --max-concurrency "$conc" --request-rate inf \
        --save-result --result-dir "$calib_dir" --result-filename "calib.json" \
        --tokenizer gpt2 --disable-tqdm > /dev/null 2>&1
    python3 -c "
import json
d = json.load(open('$calib_dir/calib.json'))
rps = d.get('request_throughput', 0)
print(max(500, min(500000, int(rps * $DURATION_SECS * 1.2))))
" 2>/dev/null || echo "2000"
}

# --- Main loop ---
for C in $CONCURRENCY_LEVELS; do
    echo ""
    echo "==============================="
    echo "  Concurrency: $C"
    echo "==============================="

    # nyann_poker
    echo "  [nyann_poker] ..."
    OUTDIR="$RESULTS_DIR/nyann_c${C}"
    mkdir -p "$OUTDIR"
    run_tool "nyann_c${C}" \
        "$NYANN" generate \
            --target "${MOCK_URL}/v1" --model mock-model \
            --config "{\"load\":{\"mode\":\"concurrent\",\"concurrency\":${C},\"duration\":\"${DURATION_SECS}s\",\"rampup\":\"2s\"},\"workload\":{\"type\":\"synthetic\",\"isl\":64,\"osl\":${MOCK_TOKENS}}}" \
            --output-dir "$OUTDIR"

    # guidellm
    echo "  [guidellm] ..."
    GDIR="$RESULTS_DIR/guidellm_out_c${C}"
    mkdir -p "$GDIR"
    run_tool "guidellm_c${C}" \
        "$GUIDELLM" benchmark run \
            --target "${MOCK_URL}" --model mock-model \
            --profile concurrent --rate "$C" --max-seconds "$DURATION_SECS" \
            --data '{"prompt_tokens": 64, "output_tokens": 32}' \
            --processor gpt2 --output-dir "$GDIR" --outputs json

    # vllm bench serve
    VLLM_PROMPTS=$(calibrate_vllm "$C")
    VDIR="$RESULTS_DIR/vllm_out_c${C}"
    mkdir -p "$VDIR"
    echo "  [vllm bench] (prompts=$VLLM_PROMPTS) ..."
    run_tool "vllm_c${C}" \
        "$VLLM_BIN" bench serve \
            --backend openai --base-url "${MOCK_URL}" --model mock-model \
            --endpoint /v1/chat/completions --dataset-name random \
            --input-len 64 --output-len "$MOCK_TOKENS" \
            --num-prompts "$VLLM_PROMPTS" --max-concurrency "$C" --request-rate inf \
            --save-result --result-dir "$VDIR" --result-filename "results.json" \
            --tokenizer gpt2 --disable-tqdm
done

# --- Summary ---
echo ""
python3 "$SCRIPT_DIR/parse_results.py" "$RESULTS_DIR" "$CONCURRENCY_LEVELS"
echo ""
echo "Full results in: $RESULTS_DIR"
