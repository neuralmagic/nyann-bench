#!/usr/bin/env python3
"""Parse benchmark results and print sustained req/s for each tool."""
import json, sys, os, glob

rdir = sys.argv[1]
levels = [int(x) for x in sys.argv[2].split()] if len(sys.argv) > 2 else [1, 4, 16, 64, 256, 1024, 4096]

hdr = f"{'Tool':<12s} {'Conc':>6s} {'Req/s':>10s} {'Requests':>10s} {'Errors':>8s} {'RSS(MB)':>10s}"
print(hdr)
print("-" * len(hdr))


def get_rss(label):
    try:
        r = json.load(open(os.path.join(rdir, f"{label}.resources.json")))
        return r["max_rss_kb"] / 1024
    except Exception:
        return 0


for c in levels:
    # nyann-bench: JSON summary on stdout
    try:
        d = json.load(open(os.path.join(rdir, f"nyann_c{c}.stdout")))
        rss = get_rss(f"nyann_c{c}")
        print(f"{'nyann':<12s} {c:>6d} {d['requests_per_second']:>10.1f} {d['total_requests']:>10d} {d['error_requests']:>8d} {rss:>10.1f}")
    except Exception as e:
        print(f"{'nyann':<12s} {c:>6d} {'FAIL':>10s}  {e}")

    # guidellm: benchmarks.json in output dir
    try:
        gfile = os.path.join(rdir, f"guidellm_out_c{c}", "benchmarks.json")
        d = json.load(open(gfile))
        b = d["benchmarks"][0]
        totals = b["metrics"]["request_totals"]
        reqs = totals["successful"]
        errs = totals["errored"]
        dur = b["duration"]
        rps = reqs / dur if dur > 0 else 0
        rss = get_rss(f"guidellm_c{c}")
        print(f"{'guidellm':<12s} {c:>6d} {rps:>10.1f} {reqs:>10d} {errs:>8d} {rss:>10.1f}")
    except Exception as e:
        print(f"{'guidellm':<12s} {c:>6d} {'FAIL':>10s}  {e}")

    # vllm bench: results.json in output dir
    try:
        vfile = os.path.join(rdir, f"vllm_out_c{c}", "results.json")
        d = json.load(open(vfile))
        rps = d.get("request_throughput", 0)
        reqs = d.get("completed", 0)
        errs = d.get("num_failed", 0)
        rss = get_rss(f"vllm_c{c}")
        print(f"{'vllm':<12s} {c:>6d} {rps:>10.1f} {reqs:>10d} {errs:>8d} {rss:>10.1f}")
    except FileNotFoundError:
        rss = get_rss(f"vllm_c{c}")
        print(f"{'vllm':<12s} {c:>6d} {'FAIL':>10s} {'':>10s} {'':>8s} {rss:>10.1f}")
    except Exception as e:
        print(f"{'vllm':<12s} {c:>6d} {'FAIL':>10s}  {e}")

    print()
