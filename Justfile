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
        --config '{"load":{"concurrency":4,"rampup":"2s","duration":"10s"},"workload":{"type":"faker","isl":32,"osl":10,"turns":3}}' \
        --output-dir /tmp/nyann_poker_smoke
    kill $SERVER_PID 2>/dev/null || true
    echo "Results:"
    wc -l /tmp/nyann_poker_smoke/requests_0.jsonl
    cat /tmp/nyann_poker_smoke/timestamps_0.json
    echo ""
    echo "Smoke test passed."

# Deploy load generation Job to Kubernetes
# CONFIG can be a file path or inline JSON string
# NAME allows running multiple jobs side-by-side (e.g. "eval" + "load")
deploy NAME TARGET CONFIG N_WORKERS='4' NAMESPACE='vllm' ARCH='arm64' OVERLAY='base' IMAGE_TAG='latest' LOG_LEVEL='info':
    #!/usr/bin/env bash
    set -euo pipefail
    kubectl -n {{NAMESPACE}} delete job {{NAME}} --ignore-not-found=true
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
        app: nyann-poker
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

# Collect JSON summaries from completed Job pods (stdout)
collect NAME NAMESPACE='vllm':
    #!/usr/bin/env bash
    set -euo pipefail
    for POD in $(kubectl -n {{NAMESPACE}} get pods -l app={{NAME}} -o jsonpath='{.items[*].metadata.name}'); do
      echo "--- $POD ---"
      kubectl -n {{NAMESPACE}} logs "$POD" -c nyann-poker
    done

# Tail logs from running Job
logs NAME NAMESPACE='vllm':
    kubectl -n {{NAMESPACE}} logs -l app={{NAME}} -c nyann-poker --tail=50 -f

clean:
    rm -f nyann_poker nyann_poker-linux-amd64 nyann_poker-linux-arm64
    rm -rf /tmp/nyann_poker_*
