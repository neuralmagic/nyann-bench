default:
    @just --list

build:
    go build -o nyann-bench ./cmd/nyann-bench/

test:
    go test ./... -count=1

test-verbose:
    go test ./... -v -count=1

# Cross-compile for linux/amd64
build-linux-amd64:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nyann-bench-linux-amd64 ./cmd/nyann-bench/

# Cross-compile for linux/arm64 (GB200, Graviton, etc.)
build-linux-arm64:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o nyann-bench-linux-arm64 ./cmd/nyann-bench/

# Build container image
container-build tag="latest":
    podman build -t nyann-bench:{{tag}} .

# Push container image to registry
container-push registry tag="latest":
    podman tag nyann-bench:{{tag}} {{registry}}/nyann-bench:{{tag}}
    podman push {{registry}}/nyann-bench:{{tag}}

# Run mock server locally
mock-server port="8000" tokens="128":
    go run ./cmd/nyann-bench/ mock-server --addr :{{port}} --output-tokens {{tokens}}

# Quick smoke test: mock server + load generator
smoke-test:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Starting mock server..."
    go run ./cmd/nyann-bench/ mock-server --addr :9999 --ttft 10ms --itl 2ms --output-tokens 20 &
    SERVER_PID=$!
    sleep 0.5
    echo "Running load generator..."
    go run ./cmd/nyann-bench/ generate \
        --target http://localhost:9999/v1 \
        --config '{"load":{"concurrency":4,"rampup":"2s","duration":"10s"},"workload":{"type":"faker","isl":32,"osl":10,"turns":3}}' \
        --output-dir /tmp/nyann-bench_smoke
    kill $SERVER_PID 2>/dev/null || true
    echo "Results:"
    wc -l /tmp/nyann-bench_smoke/requests_0.jsonl
    cat /tmp/nyann-bench_smoke/timestamps_0.json
    echo ""
    echo "Smoke test passed."

# Deploy load generation Job to Kubernetes
# CONFIG can be a file path or inline JSON string
# NAME allows running multiple jobs side-by-side (e.g. "eval" + "load")
deploy NAME TARGET CONFIG N_WORKERS='4' NAMESPACE='vllm' ARCH='arm64' OVERLAY='base' IMAGE_TAG='latest' LOG_LEVEL='info':
    #!/usr/bin/env bash
    set -euo pipefail
    kubectl -n {{NAMESPACE}} delete job {{NAME}} --ignore-not-found=true
    kubectl -n {{NAMESPACE}} delete service {{NAME}} --ignore-not-found=true
    kubectl -n {{NAMESPACE}} delete configmap {{NAME}}-config --ignore-not-found=true

    # Create ConfigMap — detect inline JSON vs file path
    CONFIG='{{CONFIG}}'
    if [[ "$CONFIG" == \{* ]]; then
      kubectl -n {{NAMESPACE}} create configmap {{NAME}}-config \
        --from-literal=config.json="$CONFIG"
    else
      kubectl -n {{NAMESPACE}} create configmap {{NAME}}-config \
        --from-file=config.json="$CONFIG"
    fi

    # Build and apply Job via kustomize + envsubst
    OVERLAY_DIR="deploy/base"
    if [[ "{{OVERLAY}}" != "base" ]]; then
      OVERLAY_DIR="deploy/overlays/{{OVERLAY}}"
    fi
    export JOB_NAME={{NAME}}
    export N_WORKERS={{N_WORKERS}}
    export TARGET={{TARGET}}
    export IMAGE_TAG={{IMAGE_TAG}}
    export ARCH={{ARCH}}
    export LOG_LEVEL={{LOG_LEVEL}}
    kubectl kustomize "$OVERLAY_DIR" | envsubst | kubectl -n {{NAMESPACE}} apply -f -

# Download a corpus and convert to flat text on Lustre
# Sources: sharegpt
prep-corpus SOURCE CORPUS_DIR NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    case "{{SOURCE}}" in
      sharegpt)
        URL="https://huggingface.co/datasets/anon8231489123/ShareGPT_Vicuna_unfiltered/resolve/main/ShareGPT_V3_unfiltered_cleaned_split.json"
        ;;
      *)
        echo "Unknown source: {{SOURCE}} (options: sharegpt)" >&2
        exit 1
        ;;
    esac
    kubectl -n {{NAMESPACE}} delete job corpus-prep --ignore-not-found=true
    CORPUS_DIR={{CORPUS_DIR}} SOURCE_URL="$URL" SOURCE_NAME={{SOURCE}} \
      envsubst < deploy/corpus-prep.yaml | kubectl -n {{NAMESPACE}} apply -f -
    echo "Waiting for job to complete..."
    kubectl -n {{NAMESPACE}} wait --for=condition=complete --timeout=600s job/corpus-prep \
      && kubectl -n {{NAMESPACE}} logs job/corpus-prep

# Download GSM8K test and train JSONL files for the gsm8k dataset type
prep-gsm8k OUTPUT_DIR NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    TEST_URL="https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/test.jsonl"
    TRAIN_URL="https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/train.jsonl"
    kubectl -n {{NAMESPACE}} delete job gsm8k-prep --ignore-not-found=true
    kubectl -n {{NAMESPACE}} apply -f - <<EOF
    apiVersion: batch/v1
    kind: Job
    metadata:
      name: gsm8k-prep
      labels:
        app: nyann-bench
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                  - matchExpressions:
                      - key: nvidia.com/gpu.present
                        operator: Exists
          containers:
            - name: download
              image: curlimages/curl:8.5.0
              securityContext:
                runAsUser: 0
              command: ["sh", "-c"]
              args:
                - |
                  mkdir -p {{OUTPUT_DIR}}
                  echo "Downloading GSM8K test split..."
                  curl -fL -o {{OUTPUT_DIR}}/gsm8k_test.jsonl "${TEST_URL}"
                  echo "Downloading GSM8K train split..."
                  curl -fL -o {{OUTPUT_DIR}}/gsm8k_train.jsonl "${TRAIN_URL}"
                  echo "Done."
                  ls -lh {{OUTPUT_DIR}}/gsm8k_*.jsonl
                  wc -l {{OUTPUT_DIR}}/gsm8k_*.jsonl
              volumeMounts:
                - mountPath: /mnt/lustre
                  name: lustre
          volumes:
            - name: lustre
              persistentVolumeClaim:
                claimName: lustre-pvc-vllm
    EOF
    echo "Waiting for job to complete..."
    kubectl -n {{NAMESPACE}} wait --for=condition=complete --timeout=120s job/gsm8k-prep \
      && kubectl -n {{NAMESPACE}} logs job/gsm8k-prep

# Download GPQA Diamond dataset (public, 198 questions) for the gpqa dataset type
prep-gpqa OUTPUT_DIR NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    kubectl -n {{NAMESPACE}} delete job gpqa-prep --ignore-not-found=true
    kubectl -n {{NAMESPACE}} apply -f - <<EOF
    apiVersion: batch/v1
    kind: Job
    metadata:
      name: gpqa-prep
      labels:
        app: nyann-bench
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                  - matchExpressions:
                      - key: nvidia.com/gpu.present
                        operator: Exists
          containers:
            - name: download
              image: alpine:3.19
              securityContext:
                runAsUser: 0
              command: ["sh", "-c"]
              args:
                - |
                  apk add --no-cache curl jq >/dev/null 2>&1
                  mkdir -p {{OUTPUT_DIR}}
                  echo "Downloading GPQA Diamond (198 questions)..."
                  curl -sf 'https://datasets-server.huggingface.co/rows?dataset=fingertap/GPQA-Diamond&config=default&split=test&offset=0&length=200' \
                    | jq -c '.rows[].row' \
                    > {{OUTPUT_DIR}}/gpqa_diamond.jsonl
                  echo "Done."
                  wc -l {{OUTPUT_DIR}}/gpqa_diamond.jsonl
                  head -1 {{OUTPUT_DIR}}/gpqa_diamond.jsonl
              volumeMounts:
                - mountPath: /mnt/lustre
                  name: lustre
          volumes:
            - name: lustre
              persistentVolumeClaim:
                claimName: lustre-pvc-vllm
    EOF
    echo "Waiting for job to complete..."
    kubectl -n {{NAMESPACE}} wait --for=condition=complete --timeout=120s job/gpqa-prep \
      && kubectl -n {{NAMESPACE}} logs job/gpqa-prep

# Collect JSON summaries from completed Job pods (stdout)
collect NAME NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    PODS=$(kubectl -n {{NAMESPACE}} get pods -l app={{NAME}} -o jsonpath='{.items[*].metadata.name}')
    for POD in $PODS; do
      ( echo "--- $POD ---"; kubectl -n {{NAMESPACE}} logs "$POD" -c nyann-bench ) &
    done
    wait

# Tail logs from running Job
logs NAME NAMESPACE='vllm':
    kubectl -n {{NAMESPACE}} logs -l app={{NAME}} -c nyann-bench --tail=50 -f

# Query Prometheus for per-stage metrics from a completed benchmark run
# PORT-FORWARD first: kubectl -n monitoring port-forward svc/prometheus 9090:9090
# Pulls timestamps from K8s pod logs by default; pass TIMESTAMPS=path for offline use
query-prometheus CLIENT_JOB DEPLOYMENT='' NAMESPACE='vllm' PROMETHEUS_URL='http://localhost:9090' TIMESTAMPS='' EVAL_JOB='':
    #!/usr/bin/env bash
    set -euo pipefail
    ARGS=(--prometheus-url {{PROMETHEUS_URL}} --client-job {{CLIENT_JOB}} -n {{NAMESPACE}})
    if [[ -n "{{DEPLOYMENT}}" ]]; then
      ARGS+=(--deployment {{DEPLOYMENT}})
    fi
    if [[ -n "{{TIMESTAMPS}}" ]]; then
      ARGS+=(--timestamps {{TIMESTAMPS}})
    fi
    if [[ -n "{{EVAL_JOB}}" ]]; then
      ARGS+=(--eval-job {{EVAL_JOB}})
    fi
    python3 scripts/query_prometheus.py "${ARGS[@]}"

# Smoke test with two local processes synchronized via barrier
smoke-test-multi:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Starting mock server..."
    go run ./cmd/nyann-bench/ mock-server --addr :9998 --ttft 10ms --itl 2ms --output-tokens 20 &
    SERVER_PID=$!
    sleep 0.5
    SYNC='{"workers":2,"addr":"localhost","timeout":"30s"}'
    echo "Starting worker 0 (leader)..."
    go run ./cmd/nyann-bench/ generate \
        --target http://localhost:9998/v1 \
        --worker-id 0 \
        --config '{"warmup":{"duration":"2s"},"load":{"concurrency":2,"duration":"5s"},"workload":{"type":"faker","isl":32,"osl":10}}' \
        --sync "$SYNC" \
        --output-dir /tmp/nyann-bench_multi_0 &
    W0_PID=$!
    sleep 0.2
    echo "Starting worker 1..."
    go run ./cmd/nyann-bench/ generate \
        --target http://localhost:9998/v1 \
        --worker-id 1 \
        --config '{"warmup":{"duration":"2s"},"load":{"concurrency":2,"duration":"5s"},"workload":{"type":"faker","isl":32,"osl":10}}' \
        --sync "$SYNC" \
        --output-dir /tmp/nyann-bench_multi_1 &
    W1_PID=$!
    wait $W0_PID $W1_PID || true
    kill $SERVER_PID 2>/dev/null || true
    echo ""
    echo "Worker 0 timestamps:"
    cat /tmp/nyann-bench_multi_0/timestamps_0.json
    echo ""
    echo "Worker 1 timestamps:"
    cat /tmp/nyann-bench_multi_1/timestamps_1.json
    echo ""
    echo "Multi-worker smoke test passed."

clean:
    rm -f nyann-bench nyann-bench-linux-amd64 nyann-bench-linux-arm64
    rm -rf /tmp/nyann-bench_*
