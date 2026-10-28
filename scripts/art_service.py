#!/usr/bin/env python3
"""jarvis-art — local photo generation service (Z-Image-Turbo, 6B).

Serves the art model behind a small HTTP API so the rest of the stack
(chat, Discord daemon, dashboard) can ask for photos:

  GET  /health    -> status, model, GPU memory, last generation
  GET  /images    -> generated images, newest first (?limit=N)
  POST /generate  -> {"prompt": ..., "seed"?: int, "size"?: "1024x1024",
                      "steps"?: int}
                     saves a PNG under ART_OUT_DIR and returns its path
                     plus timing. Generations take tens of seconds; clients
                     should allow a few minutes for the request.

The brain (llama.cpp) owns ~20 GB of the 4090's 24.5 GB and the model is
bigger than the remainder in every component, so the pipeline runs with
enable_sequential_cpu_offload(): each submodule streams to the GPU just
before its forward pass and back to RAM after. Tens of seconds per image
in exchange for near-zero resident VRAM. One generation runs at a time
(409 when busy). If the brain grows into what the art model needs at
inference time, /generate returns 503 with a CUDA OOM message instead of
crashing.

Model weights: ART_MODEL_DIR (default /var/lib/jarvis/art/model), populated
by scripts/install_art.sh. Images: ART_OUT_DIR (default /var/lib/jarvis/art/out).
"""

import os
import re
import time
import threading
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from pathlib import Path

import uvicorn
from fastapi import FastAPI, HTTPException

LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "8120"))
MODEL_DIR = os.environ.get("ART_MODEL_DIR", "/var/lib/jarvis/art/model")
OUT_DIR = os.environ.get("ART_OUT_DIR", "/var/lib/jarvis/art/out")
# Z-Image-Turbo: 9 steps = 8 DiT forwards per the model card; guidance-free
# (distilled), so guidance_scale=0.
DEFAULT_STEPS = int(os.environ.get("ART_STEPS", "9"))
MAX_DIM = int(os.environ.get("ART_MAX_DIM", "1024"))

pipe = None
_load_error = None
_lock = threading.Lock()  # one generation at a time
_busy = {"since": None}
_last = {}


def load_pipeline():
    global pipe
    import torch
    from diffusers import DiffusionPipeline

    # The brain owns ~20 GB of the 4090 (and this very session runs on it),
    # leaving only a few GB. The whole art model is bigger than that slice
    # in any single component (transformer ~11.5 GB bf16, text encoder
    # ~3.75 GB), so use sequential CPU offload: every submodule streams to
    # the GPU just before its forward pass and back to RAM after. Tens of
    # seconds per image instead of a few — acceptable for on-demand art.
    t0 = time.time()
    pipe = DiffusionPipeline.from_pretrained(MODEL_DIR, torch_dtype=torch.bfloat16)
    pipe.enable_sequential_cpu_offload()
    print(f"art: pipeline loaded in {time.time() - t0:.0f}s", flush=True)


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _load_error
    if not (Path(MODEL_DIR) / "model_index.json").exists():
        _load_error = f"model not found in {MODEL_DIR} — run scripts/install_art.sh"
        print(f"art: {_load_error}", flush=True)
    else:
        try:
            load_pipeline()
        except Exception as e:  # keep the API up so /health can explain
            _load_error = f"pipeline load failed: {e}"
            print(f"art: {_load_error}", flush=True)
    yield


app = FastAPI(title="jarvis-art", lifespan=lifespan)


def gpu_stats():
    try:
        import torch

        if torch.cuda.is_available():
            free, total = torch.cuda.mem_get_info()
            return {"free_mb": round(free / 2**20), "total_mb": round(total / 2**20)}
    except Exception:
        pass
    return None


@app.get("/health")
def health():
    return {
        "status": "ok" if pipe is not None else "no-model",
        "model": "z-image-turbo",
        "loaded": pipe is not None,
        "error": _load_error,
        "busy": _busy["since"] is not None,
        "gpu": gpu_stats(),
        "last": _last or None,
    }


@app.get("/images")
def images(limit: int = 50):
    limit = max(1, min(limit, 200))
    out = []
    for p in sorted(Path(OUT_DIR).glob("*.png"), reverse=True):
        st = p.stat()
        out.append(
            {
                "path": str(p),
                "size_bytes": st.st_size,
                "modified": datetime.fromtimestamp(st.st_mtime, timezone.utc).isoformat(),
            }
        )
        if len(out) >= limit:
            break
    return out


@app.post("/generate")
def generate(payload: dict):
    prompt = (payload.get("prompt") or "").strip()
    if not prompt:
        raise HTTPException(400, "prompt required")
    if pipe is None:
        raise HTTPException(503, _load_error or "model not loaded")
    if not _lock.acquire(blocking=False):
        raise HTTPException(409, "a generation is already running")
    _busy["since"] = time.time()
    try:
        return _run(prompt, payload)
    finally:
        _lock.release()
        _busy["since"] = None


def _run(prompt, payload):
    import torch

    size = payload.get("size") or "1024x1024"
    m = re.fullmatch(r"(\d{2,4})x(\d{2,4})", str(size))
    if not m:
        raise HTTPException(400, "size must look like 1024x1024")
    w, h = min(int(m.group(1)), MAX_DIM), min(int(m.group(2)), MAX_DIM)
    steps = int(payload.get("steps") or DEFAULT_STEPS)
    seed = (
        int(payload["seed"])
        if payload.get("seed") is not None
        else int(torch.randint(0, 2**31, (1,)).item())
    )
    gen = torch.Generator(device="cpu").manual_seed(seed)

    t0 = time.time()
    try:
        img = pipe(
            prompt,
            width=w,
            height=h,
            num_inference_steps=steps,
            guidance_scale=0.0,
            generator=gen,
        ).images[0]
    except torch.OutOfMemoryError:
        raise HTTPException(
            503, "CUDA OOM — the brain is using too much VRAM right now; try again later"
        )
    dt = time.time() - t0

    out = Path(OUT_DIR) / f"{datetime.now().strftime('%Y%m%d-%H%M%S')}-{seed}.png"
    out.parent.mkdir(parents=True, exist_ok=True)
    img.save(out)
    _last.update(
        {
            "path": str(out),
            "prompt": prompt[:200],
            "seconds": round(dt, 1),
            "at": datetime.now(timezone.utc).isoformat(),
        }
    )
    return {
        "path": str(out),
        "width": w,
        "height": h,
        "seed": seed,
        "steps": steps,
        "seconds": round(dt, 1),
    }


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=LISTEN_PORT, log_level="info")
