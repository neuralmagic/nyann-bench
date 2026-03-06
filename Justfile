default:
    @just --list

build:
    go build -o nyann_poker ./cmd/nyann_poker/

test:
    go test ./... -count=1

test-verbose:
    go test ./... -v -count=1

# Cross-compile for linux/amd64
build-linux-amd64:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nyann_poker-linux-amd64 ./cmd/nyann_poker/

# Cross-compile for linux/arm64 (GB200, Graviton, etc.)
build-linux-arm64:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o nyann_poker-linux-arm64 ./cmd/nyann_poker/

# Build container image
container-build tag="latest":
    podman build -t nyann_poker:{{tag}} .

# Push container image to registry
container-push registry tag="latest":
    podman tag nyann_poker:{{tag}} {{registry}}/nyann_poker:{{tag}}
    podman push {{registry}}/nyann_poker:{{tag}}

# Run mock server locally
mock-server port="8000" tokens="128":
    go run ./cmd/nyann_poker/ mock-server --addr :{{port}} --output-tokens {{tokens}}

# Quick smoke test: mock server + load generator
smoke-test:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Starting mock server..."
    go run ./cmd/nyann_poker/ mock-server --addr :9999 --ttft 10ms --itl 2ms --output-tokens 20 &
    SERVER_PID=$!
    sleep 0.5
    echo "Running load generator..."
    go run ./cmd/nyann_poker/ generate \
        --target http://localhost:9999/v1 \
        --concurrency 4 \
        --rampup 2s \
        --duration 10s \
        --turns 3 \
        --think-time 100ms \
        --output-dir /tmp/nyann_poker_smoke
    kill $SERVER_PID 2>/dev/null || true
    echo "Results:"
    wc -l /tmp/nyann_poker_smoke/requests_0.jsonl
    cat /tmp/nyann_poker_smoke/timestamps_0.json
    echo ""
    echo "Smoke test passed."

# Deploy load generation Job to Kubernetes
deploy TARGET MODEL CONCURRENCY='10' ISL='128' OSL='256' TURNS='1' N_WORKERS='4' RAMPUP='60' DURATION='660' NAMESPACE='vllm' ARCH='arm64':
    kubectl -n {{NAMESPACE}} delete job nyann-poker --ignore-not-found=true \
    && env \
      N_WORKERS={{N_WORKERS}} \
      TARGET={{TARGET}} \
      MODEL={{MODEL}} \
      MODE=concurrent \
      CONCURRENCY={{CONCURRENCY}} \
      RATE=10 \
      MAX_INFLIGHT=0 \
      RAMPUP={{RAMPUP}} \
      DURATION={{DURATION}} \
      DATASET=faker \
      ISL={{ISL}} \
      OSL={{OSL}} \
      TURNS={{TURNS}} \
      THINK_TIME=0s \
      IMAGE_TAG=latest \
      ARCH={{ARCH}} \
      envsubst < deploy/job.yaml | kubectl -n {{NAMESPACE}} apply -f -

# Collect JSON summaries from completed Job pods (stdout)
collect NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    for POD in $(kubectl -n {{NAMESPACE}} get pods -l app=nyann-poker -o jsonpath='{.items[*].metadata.name}'); do
      echo "--- $POD ---"
      kubectl -n {{NAMESPACE}} logs "$POD" -c nyann-poker
    done

# Tail logs from running Job
logs NAMESPACE='vllm':
    kubectl -n {{NAMESPACE}} logs -l app=nyann-poker -c nyann-poker --tail=50 -f

clean:
    rm -f nyann_poker nyann_poker-linux-amd64 nyann_poker-linux-arm64
    rm -rf /tmp/nyann_poker_*
