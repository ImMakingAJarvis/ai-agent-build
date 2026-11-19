#!/usr/bin/env python3
"""128K-context needle test for the MoE 128K suite.

Builds a deterministic ~120K-token document (repeated filler paragraphs),
inserts a unique needle line at several positions, asks the model to quote
the needle, and scores exact containment. This verifies that a 128K-context
engine (FreeToken `ft serve` with --max-seq-len 131072) actually serves the
whole window, and at what cost (wall time, generation speed) per position.

The document length is calibrated against the endpoint's own tokenizer:
one filler paragraph and ten of them are sent with max_tokens=1 and the
usage delta gives tokens-per-paragraph, so the built document lands within
a paragraph of --target-tokens. The exact prompt_tokens of every request is
reported from the API usage block.

Usage:
  python3 needle_128k.py --base-url http://127.0.0.1:11436/v1 \
      --out needle.json [--target-tokens 120000] \
      [--positions 0,25,50,75,100]

Exit code 0 = battery completed, 1 = endpoint unreachable.
"""

import argparse
import json
import random
import string
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


def make_filler(rng):
    """One deterministic filler paragraph (~60 words of plain prose)."""
    words = []
    for _ in range(60):
        words.append("".join(rng.choice(string.ascii_lowercase) for _ in range(5)))
    return " ".join(words) + "."


def calibrate(base_url, model, para):
    """Tokens per paragraph from the endpoint's own usage counts."""
    def ptoks(text):
        out = http_json(base_url.rstrip("/") + "/chat/completions", {
            "model": model,
            "messages": [{"role": "user", "content": text}],
            "max_tokens": 1, "stream": False,
        })
        return out["usage"]["prompt_tokens"]
    p1 = ptoks(para)
    p10 = ptoks(para * 10)
    tpp = (p10 - p1) / 9.0
    base = p1 - tpp  # chat-template overhead of one empty-ish message
    return tpp, base


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://127.0.0.1:11436/v1")
    ap.add_argument("--model", default=None)
    ap.add_argument("--target-tokens", type=int, default=120000)
    ap.add_argument("--positions", default="0,25,50,75,100")
    ap.add_argument("--seed", type=int, default=128)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    para = make_filler(rng)
    code = "JARVIS-" + "".join(rng.choice(string.ascii_uppercase + string.digits)
                               for _ in range(6))
    needle = f"THE NEEDLE CODE IS: {code}"

    try:
        model = args.model or pick_model(args.base_url)
        tpp, base = calibrate(args.base_url, model, para)
    except (urllib.error.URLError, TimeoutError, OSError, KeyError) as e:
        print(f"endpoint error: {type(e).__name__}: {e}", file=sys.stderr)
        return 1

    n_paras = max(10, int((args.target_tokens - base) / tpp))
    print(f"calibrated: {tpp:.2f} tok/para, base {base:.0f} tok, "
          f"{n_paras} paragraphs", flush=True)

    positions = [int(p) for p in args.positions.split(",")]
    results = []
    for pos in positions:
        # needle at pos% of the way through the paragraph list
        idx = n_paras * pos // 100
        doc = (para * idx + "\n" + needle + "\n" + para * (n_paras - idx))
        q = ("This document contains one line of the form "
             "'THE NEEDLE CODE IS: ...'. Quote the full line exactly.")
        t0 = time.time()
        try:
            out = http_json(
                args.base_url.rstrip("/") + "/chat/completions", {
                    "model": model,
                    "messages": [{"role": "user", "content": doc + "\n\n" + q}],
                    "temperature": 0, "max_tokens": 64, "stream": False,
                })
            dt = time.time() - t0
            content = out["choices"][0]["message"]["content"]
            usage = out.get("usage", {})
            gen = usage.get("completion_tokens") or 0
            rec = {
                "position": pos,
                "found": needle in content or code in content,
                "prompt_tokens": usage.get("prompt_tokens"),
                "seconds": round(dt, 2),
                "gen_tokens": gen,
                "gen_tok_s": round(gen / max(dt, 1e-9), 1),
                "reply_tail": content.strip()[-120:],
            }
        except (urllib.error.URLError, TimeoutError, OSError, KeyError) as e:
            rec = {"position": pos, "found": False,
                   "error": f"{type(e).__name__}: {e}"}
        results.append(rec)
        print(f"pos {pos:>3}%: found={rec.get('found')} "
              f"ptoks={rec.get('prompt_tokens')} s={rec.get('seconds')} "
              f"err={rec.get('error')}", flush=True)

    doc = {
        "model": model,
        "ts": time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime()),
        "needle": needle,
        "target_tokens": args.target_tokens,
        "positions": results,
        "pass_count": sum(1 for r in results if r.get("found")),
        "total": len(results),
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=2)
    print(f"PASS {doc['pass_count']}/{doc['total']} -> {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
