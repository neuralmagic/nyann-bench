#!/usr/bin/env python3
"""Query Prometheus for per-stage metrics from a nyann_poker benchmark run.

Pulls stage timestamps from K8s pod logs (or a local JSON file), merges across
pods, then queries Prometheus for server-side and client-side metrics within
each stage's time window.

Usage:
    # Port-forward Prometheus first:
    kubectl -n monitoring port-forward svc/prometheus 9090:9090 &

    # Pull timestamps from K8s pod logs:
    python3 scripts/query_prometheus.py \
        --client-job tms-sharegpt-load \
        --deployment tms-wide-ep

    # Or from a local JSON file (summary or timestamps):
    python3 scripts/query_prometheus.py \
        --client-job tms-sharegpt-load \
        --deployment tms-wide-ep \
        --timestamps /path/to/summary.json
"""

import argparse
import json
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request


# ---------------------------------------------------------------------------
# Timestamp extraction
# ---------------------------------------------------------------------------

def extract_json_from_log(log_text: str) -> dict | None:
    """Extract the last JSON object from mixed log output (stdout + stderr)."""
    lines = log_text.rstrip().split("\n")
    # Walk backwards to find the closing '}'
    end = len(lines) - 1
    while end >= 0 and lines[end].strip() != "}":
        end -= 1
    if end < 0:
        return None
    # Walk backwards counting braces to find the matching '{'
    depth = 0
    start = end
    for i in range(end, -1, -1):
        line = lines[i]
        depth += line.count("}") - line.count("{")
        if depth == 0:
            start = i
            break
    try:
        return json.loads("\n".join(lines[start : end + 1]))
    except json.JSONDecodeError:
        return None


def get_stages_from_k8s(client_job: str, namespace: str) -> list[dict]:
    """Fetch pod logs via kubectl and merge stage timestamps across pods."""
    # Get pod names
    result = subprocess.run(
        [
            "kubectl", "-n", namespace, "get", "pods",
            "-l", f"app={client_job}",
            "-o", "jsonpath={.items[*].metadata.name}",
        ],
        capture_output=True, text=True, check=True,
    )
    pods = result.stdout.strip().split()
    if not pods:
        print(f"error: no pods found with label app={client_job} in namespace {namespace}", file=sys.stderr)
        sys.exit(1)

    print(f"Found {len(pods)} pods for {client_job}", file=sys.stderr)

    # Collect per-pod stage timestamps
    all_pod_stages: list[list[dict]] = []
    for pod in pods:
        log_result = subprocess.run(
            ["kubectl", "-n", namespace, "logs", pod, "-c", "nyann-poker"],
            capture_output=True, text=True,
        )
        if log_result.returncode != 0:
            print(f"  warning: failed to get logs from {pod}", file=sys.stderr)
            continue

        summary = extract_json_from_log(log_result.stdout)
        if summary is None:
            print(f"  warning: no JSON found in logs for {pod}", file=sys.stderr)
            continue

        stages = extract_stages(summary)
        if stages:
            all_pod_stages.append(stages)
            print(f"  {pod}: {len(stages)} stages", file=sys.stderr)

    if not all_pod_stages:
        print("error: could not extract stages from any pod", file=sys.stderr)
        sys.exit(1)

    return merge_stages(all_pod_stages)


def extract_stages(data: dict) -> list[dict] | None:
    """Extract stages from a summary JSON or timestamps JSON."""
    # Could be a full summary with nested "timestamps", or a standalone timestamps file
    ts = data.get("timestamps", data)
    stages = ts.get("stages")
    if stages:
        return stages
    # Fallback: single stage from overall timestamps
    start = ts.get("rampup_end_time", ts.get("start_time"))
    end = ts.get("end_time")
    if start and end:
        return [{"stage": 0, "concurrency": 0, "start_time": start, "end_time": end}]
    return None


def merge_stages(all_pod_stages: list[list[dict]]) -> list[dict]:
    """Merge stage timestamps across pods (intersection of time windows)."""
    # Use the first pod as the reference for stage count and concurrency
    reference = all_pod_stages[0]
    merged = []
    for i, ref_stage in enumerate(reference):
        start_time = ref_stage["start_time"]
        end_time = ref_stage["end_time"]
        # Intersection: max of starts, min of ends
        for pod_stages in all_pod_stages[1:]:
            if i < len(pod_stages):
                start_time = max(start_time, pod_stages[i]["start_time"])
                end_time = min(end_time, pod_stages[i]["end_time"])
        merged.append({
            "stage": ref_stage["stage"],
            "concurrency": ref_stage["concurrency"],
            "start_time": start_time,
            "end_time": end_time,
        })
    return merged


# ---------------------------------------------------------------------------
# Prometheus queries
# ---------------------------------------------------------------------------

def prom_query(base_url: str, query: str, time: float) -> float | None:
    """Execute a Prometheus instant query and return the scalar value."""
    params = urllib.parse.urlencode({"query": query, "time": f"{time:.3f}"})
    url = f"{base_url}/api/v1/query?{params}"
    try:
        with urllib.request.urlopen(url, timeout=30) as resp:
            data = json.loads(resp.read())
    except (urllib.error.URLError, urllib.error.HTTPError) as e:
        print(f"  warning: query failed: {e}", file=sys.stderr)
        return None

    if data.get("status") != "success":
        return None

    results = data.get("data", {}).get("result", [])
    if not results:
        return None

    # Instant query returns vector; take first result's value
    val = results[0].get("value", [None, None])
    try:
        v = float(val[1])
        if v != v:  # NaN check
            return None
        return v
    except (TypeError, ValueError, IndexError):
        return None


def format_prom_duration(seconds: float) -> str:
    """Format seconds as a Prometheus range duration (e.g. '300s')."""
    s = int(seconds)
    if s < 1:
        s = 1
    return f"{s}s"


def query_stage(
    base_url: str,
    stage: dict,
    client_job: str,
    deployment: str | None,
) -> dict:
    """Run all metric queries for a single stage."""
    end_time = stage["end_time"]
    duration_s = stage["end_time"] - stage["start_time"]
    rng = format_prom_duration(duration_s)

    result = {
        "stage": stage["stage"] + 1,
        "concurrency": stage["concurrency"],
    }

    # --- Server-side metrics (vLLM) ---
    if deployment:
        pod_filter = f'pod=~"{deployment}-.*"'

        q = f"avg(vllm:num_requests_running{{{pod_filter}}})"
        result["running_requests"] = prom_query(base_url, q, end_time)

        q = f'avg(vllm:num_requests_waiting{{job="vllm-prefill",{pod_filter}}})'
        result["waiting_requests"] = prom_query(base_url, q, end_time)

        q = f'sum(rate(vllm:generation_tokens_total{{job="vllm-decode",{pod_filter}}}[{rng}]))'
        result["tgtt"] = prom_query(base_url, q, end_time)

    # --- Client-side metrics (nyann) ---
    client_filter = f'app="{client_job}"'

    for name, metric in [("ttft", "nyann_ttft_seconds_bucket"), ("itl", "nyann_itl_seconds_bucket")]:
        for pct, q_val in [("p50", 0.50), ("p95", 0.95), ("p99", 0.99)]:
            q = f"histogram_quantile({q_val}, sum(rate({metric}{{{client_filter}}}[{rng}])) by (le))"
            val = prom_query(base_url, q, end_time)
            # Convert seconds to milliseconds
            if val is not None:
                val = val * 1000
            result[f"{name}_{pct}"] = val

    # --- Eval accuracy ---
    q_correct = f"sum(rate(nyann_eval_correct{{{client_filter}}}[{rng}]))"
    q_total = f"sum(rate(nyann_eval_total{{{client_filter}}}[{rng}]))"
    correct = prom_query(base_url, q_correct, end_time)
    total = prom_query(base_url, q_total, end_time)
    if correct is not None and total is not None and total > 0:
        result["eval_accuracy"] = correct / total
    else:
        result["eval_accuracy"] = None

    return result


# ---------------------------------------------------------------------------
# Output formatting
# ---------------------------------------------------------------------------

def fmt(val, precision=1):
    """Format a value for display."""
    if val is None:
        return ""
    if isinstance(val, float):
        return f"{val:.{precision}f}"
    return str(val)


def print_table(rows: list[dict], deployment: str | None):
    """Print results as a tab-separated table."""
    columns = [
        ("Stage", "stage", 0),
        ("Concurrency", "concurrency", 0),
    ]
    if deployment:
        columns += [
            ("Running Reqs", "running_requests", 1),
            ("Waiting Reqs", "waiting_requests", 1),
            ("TGTT", "tgtt", 0),
        ]
    columns += [
        ("TTFT p50 (ms)", "ttft_p50", 1),
        ("TTFT p95", "ttft_p95", 1),
        ("TTFT p99", "ttft_p99", 1),
        ("ITL p50 (ms)", "itl_p50", 1),
        ("ITL p95", "itl_p95", 1),
        ("ITL p99", "itl_p99", 1),
    ]

    # Check if any row has eval data
    has_eval = any(r.get("eval_accuracy") is not None for r in rows)
    if has_eval:
        columns.append(("Eval Acc", "eval_accuracy", 3))

    # Header
    print("\t".join(c[0] for c in columns))

    # Rows
    for row in rows:
        cells = []
        for _, key, prec in columns:
            cells.append(fmt(row.get(key), prec))
        print("\t".join(cells))


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Query Prometheus for per-stage nyann_poker benchmark metrics."
    )
    parser.add_argument(
        "--prometheus-url",
        default="http://localhost:9090",
        help="Prometheus base URL (default: http://localhost:9090)",
    )
    parser.add_argument(
        "--client-job",
        required=True,
        help="nyann_poker K8s Job name (used as app label for client pod filtering)",
    )
    parser.add_argument(
        "--namespace", "-n",
        default="vllm",
        help="K8s namespace for pod log collection (default: vllm)",
    )
    parser.add_argument(
        "--deployment",
        default=None,
        help="vLLM deployment name for server-side metrics (pod name prefix)",
    )
    parser.add_argument(
        "--timestamps",
        default=None,
        help="Path to a JSON file (summary or timestamps) instead of fetching from K8s",
    )
    parser.add_argument(
        "--json",
        action="store_true",
        dest="json_output",
        help="Output as JSON instead of table",
    )
    args = parser.parse_args()

    # Get stage timestamps
    if args.timestamps:
        with open(args.timestamps) as f:
            data = json.load(f)
        stages = extract_stages(data)
        if not stages:
            print("error: no stage timestamps found in file", file=sys.stderr)
            sys.exit(1)
    else:
        stages = get_stages_from_k8s(args.client_job, args.namespace)

    print(
        f"Querying {len(stages)} stages from {args.prometheus_url}...",
        file=sys.stderr,
    )

    rows = []
    for stage in stages:
        duration = stage["end_time"] - stage["start_time"]
        print(
            f"  Stage {stage['stage'] + 1}: concurrency={stage['concurrency']} "
            f"duration={duration:.0f}s",
            file=sys.stderr,
        )
        row = query_stage(args.prometheus_url, stage, args.client_job, args.deployment)
        rows.append(row)

    if args.json_output:
        print(json.dumps(rows, indent=2))
    else:
        print_table(rows, args.deployment)


if __name__ == "__main__":
    main()
