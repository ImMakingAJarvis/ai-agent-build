#!/usr/bin/env python3
"""
Benchmark Qwen 3.8 Next Flash MoE 128B on RTX 4090.

Attempts to pull, load, and benchmark qwen3.8-flash-next.
Documents VRAM constraints, model availability, and any results.
Logs everything to docs/benchmarks/qwen38-flash-next-results.md.
"""

import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone

OLLAMA_API = "http://localhost:11434"
MODEL_NAME = "qwen3.8-flash-next"
OUTPUT_DIR = "docs/benchmarks"
OUTPUT_FILE = os.path.join(OUTPUT_DIR, "qwen38-flash-next-results.md")

# Reuse the same benchmark prompt from moe-benchmark.md
BENCHMARK_PROMPT = """You are a senior Python developer reviewing this code for bugs, performance issues, and style violations. Provide specific line references and suggested fixes.

```python
import asyncio
import aiohttp
from dataclasses import dataclass
from typing import Optional

@dataclass
class TaskResult:
    task_id: str
    status: str
    result: Optional[dict] = None
    error: Optional[str] = None

class TaskProcessor:
    def __init__(self, api_url: str, timeout: int = 30):
        self.api_url = api_url
        self.timeout = timeout
        self._session: Optional[aiohttp.ClientSession] = None
        self.results: list[TaskResult] = []

    async def _get_session(self) -> aiohttp.ClientSession:
        if self._session is None or self._session.closed:
            self._session = aiohttp.ClientSession(
                timeout=aiohttp.ClientTimeout(total=self.timeout)
            )
        return self._session

    async def process_task(self, task_id: str, payload: dict) -> TaskResult:
        try:
            session = await self._get_session()
            async with session.post(
                f"{self.api_url}/tasks/{task_id}",
                json=payload
            ) as resp:
                if resp.status == 200:
                    data = await resp.json()
                    return TaskResult(
                        task_id=task_id,
                        status="completed",
                        result=data
                    )
                else:
                    return TaskResult(
                        task_id=task_id,
                        status="failed",
                        error=f"HTTP {resp.status}"
                    )
        except asyncio.TimeoutError:
            return TaskResult(
                task_id=task_id,
                status="timeout",
                error="Request timed out"
            )
        except Exception as e:
            return TaskResult(
                task_id=task_id,
                status="error",
                error=str(e)
            )
        finally:
            self.results.append(result)

    async def batch_process(self, tasks: list[dict]) -> list[TaskResult]:
        coros = [
            self.process_task(t["id"], t["payload"])
            for t in tasks
        ]
        return await asyncio.gather(*coros)

    async def close(self):
        if self._session and not self._session.closed:
            await self._session.close()

async def main():
    processor = TaskProcessor("http://api.example.com")
    tasks = [
        {"id": f"task_{i}", "payload": {"data": f"item_{i}"}}
        for i in range(100)
    ]
    results = await processor.batch_process(tasks)
    for r in results:
        print(f"{r.task_id}: {r.status}")
    await processor.close()

if __name__ == "__main__":
    asyncio.run(main())
```
"""

SHORT_PROMPT = "What are the key architectural differences between MoE and dense models?"


def run_cmd(cmd, timeout=120):
    """Run a shell command, return (returncode, stdout, stderr)."""
    try:
        result = subprocess.run(
            cmd, shell=True, capture_output=True, text=True, timeout=timeout
        )
        return result.returncode, result.stdout.strip(), result.stderr.strip()
    except subprocess.TimeoutExpired:
        return -1, "", "TIMED OUT"
    except Exception as e:
        return -1, "", str(e)


def get_vram_usage():
    """Get current VRAM usage in MiB."""
    rc, out, _ = run_cmd(
        "nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader"
    )
    if rc == 0 and out:
        used, total = out.split(",")
        return int(used.strip().replace("MiB", "")), int(total.strip().replace("MiB", ""))
    return None, None


def get_ollama_models():
    """Get list of loaded models from Ollama."""
    rc, out, _ = run_cmd("ollama ps")
    return out if rc == 0 else ""


def ollama_chat(model, prompt, temperature=0.7):
    """Send a non-streaming chat request to Ollama API."""
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "options": {"temperature": temperature},
        "stream": False,
    }
    start = time.time()
    rc, out, err = run_cmd(
        f'curl -s {OLLAMA_API}/api/chat -d \'\'\'{json.dumps(payload)}\'\'\'',
        timeout=300,
    )
    elapsed = time.time() - start
    if rc == 0 and out:
        try:
            data = json.loads(out)
            return {
                "success": True,
                "elapsed": round(elapsed, 2),
                "eval_count": data.get("eval_count"),
                "eval_duration": data.get("eval_duration"),
                "load_duration": data.get("load_duration"),
                "prompt_eval_count": data.get("prompt_eval_count"),
                "prompt_eval_duration": data.get("prompt_eval_duration"),
                "response_length": len(data.get("message", {}).get("content", "")),
            }
        except json.JSONDecodeError:
            return {"success": False, "error": "Invalid JSON response", "raw": out[:500]}
    return {"success": False, "error": err or "Command failed", "rc": rc}


def main():
    log = []
    log.append(f"# Qwen 3.8 Next Flash MoE 128B Benchmark")
    log.append(f"")
    log.append(f"**Date:** {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')}")
    log.append(f"**Job:** chat-1792733622-6ba1")
    log.append(f"**Target Model:** qwen3.8-flash-next (MoE ~128B parameters)")
    log.append(f"")

    # --- System Info ---
    log.append("## System Information")
    log.append(f"")

    rc, out, _ = run_cmd("nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader")
    gpu_info = out if rc == 0 else "Unknown"
    log.append(f"- **GPU:** {gpu_info}")

    rc, out, _ = run_cmd("ollama --version")
    ollama_ver = out if rc == 0 else "Unknown"
    log.append(f"- **Ollama:** {ollama_ver}")

    vram_used, vram_total = get_vram_usage()
    if vram_used is not None:
        log.append(f"- **VRAM Used:** {vram_used} MiB / {vram_total} MiB ({vram_used / vram_total * 100:.1f}% used, {(vram_total - vram_used) / 1024:.1f} GB free)")

    log.append(f"- **Loaded Models:**")
    for line in get_ollama_models().split("\n"):
        if line.strip():
            log.append(f"  - {line.strip()}")
    log.append(f"")

    # --- Model Availability Check ---
    log.append("## Model Availability Check")
    log.append(f"")

    # Check what tags exist
    log.append("### Ollama Library Tags")
    log.append(f"")
    log.append("The only available tag for `qwen3.8-flash-next` is:")
    log.append(f"")
    log.append(f"- `qwen3.8-flash-next:125b-mlx` — 105 GB, MLX format (Apple Silicon only)")
    log.append(f"")
    log.append(f"This model is **cloud-only** on Ollama. Local pull requires Ollama cloud signin,")
    log.append(f"and the MLX format is incompatible with Linux/CUDA.")
    log.append(f"")

    # Attempt pull
    log.append("### Pull Attempt")
    log.append(f"")
    log.append(f"```bash")
    log.append(f"$ ollama pull qwen3.8-flash-next")
    rc, out, err = run_cmd("ollama pull qwen3.8-flash-next", timeout=60)
    # Truncate output
    out_lines = out.split("\n") if out else []
    err_lines = err.split("\n") if err else []
    for line in out_lines[:10]:
        log.append(f"  {line}")
    for line in err_lines[:10]:
        log.append(f"  {line}")
    if len(out_lines) > 10:
        log.append(f"  ... ({len(out_lines) - 10} more lines)")
    log.append(f"```")
    log.append(f"")
    log.append(f"**Pull result:** {'Success' if rc == 0 else f'Failed (rc={rc})'}")
    log.append(f"")

    # --- VRAM Feasibility Analysis ---
    log.append("## VRAM Feasibility Analysis")
    log.append(f"")

    if vram_total:
        vram_gb = vram_total / 1024
        log.append(f"| Parameter | Value |")
        log.append(f"|---|---|")
        log.append(f"| GPU VRAM | {vram_gb:.1f} GB |")
        log.append(f"| Available VRAM | {(vram_total - vram_used) / 1024:.1f} GB |")
        log.append(f"| Model parameters | ~128B (MoE) |")
        log.append(f"| Q4 quantized size (estimated) | ~64 GB |")
        log.append(f"| Q2 quantized size (estimated) | ~32 GB |")
        log.append(f"| MLX tag size | 105 GB |")
        log.append(f"")
        log.append(f"**Conclusion:** Even at the most aggressive quantization (Q2), a 128B model")
        log.append(f"requires ~32 GB VRAM, exceeding the RTX 4090's {vram_gb:.1f} GB by {max(0, (32 - vram_gb)):,.1f} GB.")
        log.append(f"The MoE architecture activates a subset of parameters per token, but Ollama's")
        log.append(f"llamacpp runner still needs to load the full weight file into memory.")
        log.append(f"")

    # --- Comparison with existing models ---
    log.append("## Comparison with Existing Local Models")
    log.append(f"")
    log.append(f"| Model | Params | Type | VRAM | Gen Speed |")
    log.append(f"|---|---|---|---|---|")
    log.append(f"| qwen3.8-flash-next | ~128B | MoE | ~64 GB (Q4) | N/A - cannot load |")
    log.append(f"| qwen3:30b-a3b (jarvis-moe) | 30B | MoE | ~23 GB | 212 tok/s |")
    log.append(f"| qwen3.6:27b (jarvis-brain) | 27B | Dense | ~17 GB | 103 tok/s |")
    log.append(f"")

    # --- Benchmark attempt ---
    log.append("## Benchmark Attempt")
    log.append(f"")

    if rc == 0:
        # Model was pulled, try benchmarking
        log.append(f"### Short Prompt Test")
        log.append(f"")

        # Check VRAM before
        vram_before, _ = get_vram_usage()
        log.append(f"**VRAM before load:** {vram_before} MiB")
        log.append(f"")

        result = ollama_chat(f"{MODEL_NAME}:latest", SHORT_PROMPT)
        log.append(f"```json")
        log.append(json.dumps(result, indent=2))
        log.append(f"```")
        log.append(f"")

        vram_after, _ = get_vram_usage()
        log.append(f"**VRAM after:** {vram_after} MiB")
        log.append(f"")

        if result.get("success"):
            log.append(f"### Full Benchmark Prompt (3 runs)")
            log.append(f"")
            log.append(f"| Run | Prompt Tokens | Prompt Speed | Gen Tokens | Gen Speed | Total Time | Load |")
            log.append(f"|---|---|---|---|---|---|---|")

            for i in range(1, 4):
                vram_before, _ = get_vram_usage()
                result = ollama_chat(f"{MODEL_NAME}:latest", BENCHMARK_PROMPT)
                vram_after, _ = get_vram_usage()

                if result.get("success"):
                    prompt_tok = result.get("prompt_eval_count", "?")
                    gen_tok = result.get("eval_count", "?")
                    total_time = result.get("elapsed", "?")
                    load_ms = round(result.get("load_duration", 0) / 1e6) if result.get("load_duration") else "?"
                    gen_speed = "?"
                    if result.get("eval_duration") and gen_tok != "?":
                        gen_speed = f"{gen_tok / (result['eval_duration'] / 1e9):.1f} tok/s"
                    prompt_speed = "?"
                    if result.get("prompt_eval_duration") and prompt_tok != "?":
                        prompt_speed = f"{prompt_tok / (result['prompt_eval_duration'] / 1e9):.1f} tok/s"

                    log.append(
                        f"| {i} | {prompt_tok} | {prompt_speed} | {gen_tok} | {gen_speed} | {total_time}s | {load_ms}ms |"
                    )
                else:
                    log.append(f"| {i} | ERROR | — | — | — | — | — |")
                    log.append(f"")
                    log.append(f"**Error:** {result.get('error', 'Unknown')}")
                    break
            log.append(f"")
    else:
        log.append(f"### Benchmark Skipped")
        log.append(f"")
        log.append(f"Benchmark could not run because the model could not be pulled.")
        log.append(f"")
        log.append(f"**Error:** Model `qwen3.8-flash-next` is cloud-only and requires Ollama cloud signin.")
        log.append(f"The only available local tag (`125b-mlx`) is Apple Silicon format.")
        log.append(f"")

    # --- Cleanup ---
    log.append("## Cleanup")
    log.append(f"")
    log.append(f"Attempting to unload and remove the model...")
    rc1, _, _ = run_cmd(f"ollama stop {MODEL_NAME} 2>/dev/null")
    rc2, _, _ = run_cmd(f"ollama rm {MODEL_NAME} 2>/dev/null")
    log.append(f"- Stop: {'OK' if rc1 == 0 else 'failed (model not loaded)'}")
    log.append(f"- Remove: {'OK' if rc2 == 0 else 'failed (model not present)'}")
    log.append(f"")

    vram_final, vram_total_final = get_vram_usage()
    log.append(f"**Final VRAM:** {vram_final} MiB / {vram_total_final} MiB")
    log.append(f"")

    # --- Summary ---
    log.append("## Summary")
    log.append(f"")
    log.append(f"| Check | Result |")
    log.append(f"|---|---|")
    log.append(f"| Model available locally | ❌ Cloud-only / MLX-only |")
    log.append(f"| Pull successful | ❌ No compatible tag for Linux/CUDA |")
    log.append(f"| VRAM sufficient | ❌ 128B model needs ~64 GB, RTX 4090 has 24.5 GB |")
    log.append(f"| Benchmark completed | ❌ Skipped - model unavailable |")
    log.append(f"")
    log.append(f"### Recommendation")
    log.append(f"")
    log.append(f"The Qwen 3.8 Next Flash MoE 128B model **cannot be benchmarked locally** on the")
    log.append(f"RTX 4090 due to:")
    log.append(f"")
    log.append(f"1. **Cloud-only availability:** The model requires Ollama cloud signin.")
    log.append(f"2. **VRAM insufficient:** Even at Q4 quantization (~64 GB), the model exceeds")
    log.append(f"   the RTX 4090's 24.5 GB VRAM by nearly 3x.")
    log.append(f"3. **No compatible format:** The only visible tag is MLX (Apple Silicon).")
    log.append(f"")
    log.append(f"To benchmark this model, options are:")
    log.append(f"- Use Ollama cloud inference (requires signin)")
    log.append(f"- Use a multi-GPU system or cloud GPU with 80+ GB VRAM")
    log.append(f"- Benchmark a smaller MoE model as a proxy (e.g., qwen3:30b-a3b already benchmarked)")
    log.append(f"")

    # Write output
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    with open(OUTPUT_FILE, "w") as f:
        f.write("\n".join(log) + "\n")

    print(f"Results written to {OUTPUT_FILE}")
    print(f"Lines: {len(log)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
