#!/usr/bin/env python3
"""Benchmark the live llama.cpp brain (Qwen3.8 27B Q6_K) via /v1/chat/completions.

Usage: benchmark_q6.py [base_url] [runs] [fill_tokens]
fill_tokens>0 adds a long-context run: a prompt of about that many tokens ending in a question.
Reports llama-server's own timings (prompt/decode tok/s, draft acceptance), plus
a tool-call check and a few small correctness tasks.
"""
import json, sys, time, urllib.error, urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:11434"
RUNS = int(sys.argv[2]) if len(sys.argv) > 2 else 3
FILL = int(sys.argv[3]) if len(sys.argv) > 3 else 0

Usage: benchmark_q6.py [base_url] [runs]
Reports llama-server's own timings (prompt/decode tok/s, draft acceptance), plus
a tool-call check and a few small correctness tasks.

REVIEW = "Review this Python code for bugs, performance and style, and propose a fixed version:\n\n" + '''
import os, json, threading
cache = {}
lock = threading.Lock()
def load(path):
    f = open(path)
    data = json.loads(f.read())
    return data
def get(path):
    if path in cache:
        return cache[path]
    data = load(path)
    cache[path] = data
    return data
def walk(root):
    out = []
    for d, _, files in os.walk(root):
        for n in files:
            if n.endswith(".json"):
                out.append(get(d + "/" + n))
    return out
def merge(items):
    res = {}
    for it in items:
        for k, v in it.items():
            if k in res and type(v) == list:
                res[k] = res[k] + v
            else:
                res[k] = v
    return res
''' * 3

TASKS = [  # (prompt, checker)
    ("Reply with only the number: what is 17*23 + 4?", lambda s: "395" in s),
    ("Write a Python function is_palindrome(s) ignoring case and non-alphanumerics. Output only a code block.",
     lambda s: "def is_palindrome" in s),
    ("Reply with only the word: what is the capital of Australia?", lambda s: "canberra" in s.lower()),
]

TOOL = {"type": "function", "function": {"name": "bash", "description": "Run a shell command",
        "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}}


def chat(messages, **kw):
    body = {"messages": messages, "temperature": 0.6, "top_p": 0.95, "top_k": 20, "max_tokens": 1024, "cache_prompt": False, **kw}
    req = urllib.request.Request(BASE + "/v1/chat/completions", json.dumps(body).encode(),
                                 {"Content-Type": "application/json"})
    t = time.time()
    try:
        r = json.load(urllib.request.urlopen(req, timeout=1800))
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode()[:300]}")
    return r, time.time() - t


def main():
    print("| Run | Prompt tok | Prompt tok/s | Gen tok | Gen tok/s | Wall s |\n|---|---|---|---|---|---|")
    for i in range(RUNS):
        r, wall = chat([{"role": "user", "content": REVIEW}])
        t = r["timings"]
        print(f"| {i+1} | {t['prompt_n']} | {t['prompt_per_second']:.0f} | {t['predicted_n']} | "
              f"{t['predicted_per_second']:.1f} | {wall:.1f} |")
    print()
    for p, ok in TASKS:
        r, wall = chat([{"role": "user", "content": p}])
        out = r["choices"][0]["message"].get("content") or ""
        print(f"task {'PASS' if ok(out) else 'FAIL'} ({r['timings']['predicted_n']} tok, {wall:.1f}s): {p[:50]}")
    r, wall = chat([{"role": "user", "content": "List the files in /tmp using the bash tool."}], tools=[TOOL])
    tc = r["choices"][0]["message"].get("tool_calls") or []
    good = bool(tc) and tc[0]["function"]["name"] == "bash" and "command" in json.loads(tc[0]["function"]["arguments"])
    print(f"tool_call {'PASS' if good else 'FAIL'}: {json.dumps(tc)[:200]}")

    if FILL:
        # ~10 tokens per line; the needle sits at the very start so the whole context must be attended.
        lines = ["SECRET CODE: tangerine-4471. Remember it."] + [f"Log line {i}: worker {i % 97} processed batch {i * 7 % 1013} ok." for i in range(FILL // 24)]
        r, wall = chat([{"role": "user", "content": "\n".join(lines) + "\n\nWhat was the SECRET CODE at the start? Reply with only the code."}], max_tokens=256)
        t = r["timings"]
        out = r["choices"][0]["message"].get("content") or ""
        print(f"\nlong-context: prompt {t['prompt_n']} tok at {t['prompt_per_second']:.0f} tok/s, "
              f"decode {t['predicted_per_second']:.1f} tok/s, wall {wall:.1f}s, needle {'PASS' if 'tangerine-4471' in out else 'FAIL'}")


main()
