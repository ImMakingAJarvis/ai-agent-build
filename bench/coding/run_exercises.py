#!/usr/bin/env python3
"""Coding-exercise battery for the MoE 128K suite.

Sends each exercise's task to an OpenAI-compatible /chat/completions
endpoint (the FreeToken `ft serve` box on :11436), extracts the first
```python fenced block from the reply, and grades it by running the
exercise's tests.py in a temp dir.

Usage:
  python3 run_exercises.py --base-url http://127.0.0.1:11436/v1 \
      --out results.json [--model NAME] [--max-tokens 2048] [--temp 0.2]

Exit code 0 = battery completed (some exercises may have failed),
1 = the endpoint was unreachable or no exercises could be run.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
EXERCISES = os.path.join(HERE, "exercises")

FENCE = re.compile(r"```(?:python)?\s*\n(.*?)```", re.DOTALL)


def http_json(url, payload=None, timeout=600):
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


def extract_code(content):
    m = FENCE.search(content or "")
    if m:
        return m.group(1)
    return content or ""


def run_exercise(ex_dir, base_url, model, max_tokens, temp, gen_timeout):
    name = os.path.basename(ex_dir)
    with open(os.path.join(ex_dir, "task.md")) as f:
        task = f.read()
    prompt = (
        task
        + "\n\nRespond with exactly one fenced ```python code block "
        "containing the complete solution in a file named solution.py. "
        "No prose outside the code block."
    )
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": temp,
        "max_tokens": max_tokens,
        "stream": False,
    }
    rec = {"name": name, "passed": False, "error": None,
           "gen_seconds": None, "gen_tokens": None, "tests_tail": None}
    try:
        t0 = time.time()
        out = http_json(base_url.rstrip("/") + "/chat/completions", payload,
                        timeout=gen_timeout)
        rec["gen_seconds"] = round(time.time() - t0, 1)
        content = out["choices"][0]["message"]["content"]
        if out.get("usage", {}).get("completion_tokens") is not None:
            rec["gen_tokens"] = out["usage"]["completion_tokens"]
        code = extract_code(content)
        if not code.strip():
            rec["error"] = "empty reply"
            return rec

        tmp = tempfile.mkdtemp(prefix="moecode-")
        try:
            with open(os.path.join(tmp, "solution.py"), "w") as f:
                f.write(code)
            shutil.copy(os.path.join(ex_dir, "tests.py"), tmp)
            p = subprocess.run(
                [sys.executable, "tests.py"], cwd=tmp,
                capture_output=True, text=True, timeout=120,
            )
            rec["passed"] = p.returncode == 0
            tail = (p.stdout + p.stderr).strip().splitlines()
            rec["tests_tail"] = tail[-3:] if tail else None
            if not rec["passed"]:
                rec["error"] = "tests failed"
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
    except (urllib.error.URLError, TimeoutError, OSError, KeyError,
            ValueError) as e:
        rec["error"] = f"{type(e).__name__}: {e}"
    return rec


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://127.0.0.1:11436/v1")
    ap.add_argument("--model", default=None,
                    help="model id (default: first id from /models)")
    ap.add_argument("--out", required=True, help="results JSON path")
    ap.add_argument("--max-tokens", type=int, default=2048)
    ap.add_argument("--temp", type=float, default=0.2)
    ap.add_argument("--gen-timeout", type=int, default=600)
    args = ap.parse_args()

    model = args.model or pick_model(args.base_url)
    results = []
    for d in sorted(os.listdir(EXERCISES)):
        ex_dir = os.path.join(EXERCISES, d)
        if not os.path.isdir(ex_dir):
            continue
        print(f"== {d} ==", flush=True)
        rec = run_exercise(ex_dir, args.base_url, model, args.max_tokens,
                           args.temp, args.gen_timeout)
        print(f"   passed={rec['passed']} gen={rec['gen_seconds']}s "
              f"err={rec['error']}", flush=True)
        results.append(rec)

    passed = sum(1 for r in results if r["passed"])
    doc = {
        "base_url": args.base_url,
        "model": model,
        "ts": time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime()),
        "pass_count": passed,
        "total": len(results),
        "exercises": results,
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=2)
    print(f"PASS {passed}/{len(results)} -> {args.out}")
    if not results:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
