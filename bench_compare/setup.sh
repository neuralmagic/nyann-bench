#!/usr/bin/env bash
# Install competitor benchmarking tools in isolated uv venvs.
# Requires: uv, python 3.12
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENVS="$SCRIPT_DIR/venvs"
mkdir -p "$VENVS"

echo "=== Installing guidellm ==="
uv venv "$VENVS/guidellm" --python 3.12
VIRTUAL_ENV="$VENVS/guidellm" uv pip install guidellm --python "$VENVS/guidellm/bin/python"

echo ""
echo "=== Installing vllm (for vllm bench serve) ==="
uv venv "$VENVS/vllm" --python 3.12
VIRTUAL_ENV="$VENVS/vllm" uv pip install vllm --python "$VENVS/vllm/bin/python"

echo ""
echo "=== Done ==="
echo "guidellm:   $VENVS/guidellm/bin/guidellm"
echo "vllm bench: $VENVS/vllm/bin/vllm bench serve"
