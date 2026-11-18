#!/usr/bin/env python3
"""Standard-prompt 3-run benchmark for the MoE 128K suite.

Same measurement as the earlier VRAM sweeps (see
docs/benchmarks/qwen38-flash-next-vram-sweep-250mb.md): each run makes
two calls — a max_tokens=1 call whose wall time bounds the prompt
(prep) speed, then the real max_tokens=256 call; generation tok/s is
256 / (total - prefill).

Usage:
  python3 run_standard.py --base-url http://127.0.0.1:11436/v1 \
      --prompt docs/benchmarks/moe-sweep-prompt.txt --out standard.json

Prints a markdown table row set suitable for pasting into the results
doc, and writes the raw numbers to --out.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request


def http_json(url, payload=None, timeout=1800):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def pick_model(base_url):
    models = http_json(base_url.rstrip("/") + "/models", timeout=30)
    data = models.get("data", [])
    return data[0]["id"] if data else "default"


def chat(base_url, model, prompt, max_tokens):
    t0 = time.time()
    out = http_json(base_url.rstrip("/") + "/chat/completions", {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0.7,
        "max_tokens": max_tokens,
        "stream": False,
    })
    dt = time.time() - t0
    usage = out.get("usage", {})
    return {
        "seconds": dt,
        "prompt_tokens": usage.get("prompt_tokens"),
        "gen_tokens": usage.get("completion_tokens"),
        "content_len": len(out["choices"][0]["message"]["content"]),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://127.0.0.1:11436/v1")
    ap.add_argument("--prompt", required=True)
    ap.add_argument("--model", default=None)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    with open(args.prompt) as f:
        prompt = f.read()
    model = args.model or pick_model(args.base_url)

    rows = []
    for i in (1, 2, 3):
        label = "1 (cold)" if i == 1 else str(i)
        try:
            pre = chat(args.base_url, model, prompt, 1)
            tot = chat(args.base_url, model, prompt, 256)
            ptok = pre["prompt_tokens"]
            pt = ptok / pre["seconds"] if pre["seconds"] > 0 else None
            gt = (tot["gen_tokens"] or 0) / max(tot["seconds"] - pre["seconds"], 1e-9)
            rows.append({"run": label, "prompt_tokens": ptok,
                         "prompt_tok_s": round(pt, 1), "gen_tokens": tot["gen_tokens"],
                         "gen_tok_s": round(gt, 1), "total_s": round(tot["seconds"], 2)})
            print(f"| {label} | {ptok} | {round(pt, 1)} | {tot['gen_tokens']} | "
                  f"{round(gt, 1)} | {round(tot['seconds'], 2)} s |", flush=True)
        except (urllib.error.URLError, TimeoutError, OSError, KeyError) as e:
            rows.append({"run": label, "error": f"{type(e).__name__}: {e}"})
            print(f"| {label} | ERROR {e} |", flush=True)

    with open(args.out, "w") as f:
        json.dump({"model": model, "ts": time.strftime("%Y-%m-%d %H:%M:%S UTC",
                    time.gmtime()), "runs": rows}, f, indent=2)
    print(f"-> {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
