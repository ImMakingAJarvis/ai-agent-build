#!/usr/bin/env python3
"""jarvis-art — local photo generation service (Z-Image-Turbo, 6B).

Serves the art model behind a small HTTP API so the rest of the stack
(chat, Discord daemon, dashboard) can ask for photos:

  GET  /          -> the gallery: a page showing every generated image
  GET  /health    -> status, model, GPU memory, last generation
  GET  /images    -> generated images, newest first (?limit=N), each with
                     name, prompt, seed and timing (from the sidecar .json
                     written next to each PNG)
  GET  /img/N.png -> the image file itself (basename only, no traversal)
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

import json
import os
import re
import time
import threading
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from pathlib import Path

import uvicorn
from fastapi import FastAPI, HTTPException
from fastapi.responses import FileResponse, HTMLResponse

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

# The gallery: a self-contained page (no external assets) that fetches
# /images and renders every generated photo with its prompt and timing.
# It re-fetches every 30 s, so a new photo appears without reloading.
GALLERY_PAGE = """<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>jarvis-art — generated photos</title>
<style>
  :root { color-scheme: dark; }
  body { margin: 0; background: #0f1115; color: #e6e8ee;
         font: 15px/1.5 system-ui, -apple-system, sans-serif; }
  header { padding: 16px 22px; border-bottom: 1px solid #262a33;
           display: flex; align-items: baseline; gap: 12px; }
  h1 { font-size: 18px; margin: 0; }
  #count { color: #8b93a7; font-size: 13px; }
  #grid { display: grid; gap: 14px; padding: 18px 22px;
          grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); }
  .card { background: #171a21; border: 1px solid #262a33;
          border-radius: 10px; overflow: hidden; }
  .card img { width: 100%; display: block; cursor: zoom-in; }
  .cap { padding: 8px 10px 10px; font-size: 13px; }
  .cap .p { display: -webkit-box; -webkit-line-clamp: 2;
            -webkit-box-orient: vertical; overflow: hidden; }
  .cap .m { color: #8b93a7; font-size: 12px; margin-top: 4px; }
  #empty { padding: 48px; text-align: center; color: #8b93a7; }
</style>
</head>
<body>
<header><h1>jarvis-art</h1><span id="count"></span></header>
<div id="grid"></div>
<div id="empty" hidden>No photos yet — ask Jarvis on Discord:
<code>!photo a photo of ...</code></div>
<script>
async function refresh() {
  try {
    const r = await fetch('/images?limit=200');
    if (!r.ok) return;
    const items = await r.json();
    const grid = document.getElementById('grid');
    document.getElementById('count').textContent =
      items.length + ' photo' + (items.length === 1 ? '' : 's');
    document.getElementById('empty').hidden = items.length > 0;
    grid.innerHTML = '';
    for (const it of items) {
      const card = document.createElement('div'); card.className = 'card';
      const img = document.createElement('img');
      img.loading = 'lazy';
      img.src = '/img/' + encodeURIComponent(it.name);
      img.alt = it.prompt || 'generated photo';
      const cap = document.createElement('div'); cap.className = 'cap';
      const p = document.createElement('div'); p.className = 'p';
      p.textContent = it.prompt || '(no prompt saved)';
      const m = document.createElement('div'); m.className = 'm';
      m.textContent = [
        it.modified ? it.modified.replace('T', ' ').slice(0, 16) + ' UTC' : null,
        it.seed != null ? 'seed ' + it.seed : null,
        it.seconds != null ? it.seconds + 's' : null,
      ].filter(Boolean).join(' \\u00b7 ');
      cap.append(p, m); card.append(img, cap); grid.append(card);
    }
  } catch (e) { /* service restarting; try again next tick */ }
}
refresh();
setInterval(refresh, 30000);
</script>
</body>
</html>
"""


def read_meta(png: Path) -> dict:
    """Read the sidecar <name>.json written next to each generated PNG.

    Images made before the sidecar existed (or a missing/corrupt file)
    simply come back without prompt/seed fields.
    """
    try:
        with open(png.with_suffix(".json")) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


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
        item = {
            "path": str(p),
            "name": p.name,
            "size_bytes": st.st_size,
            "modified": datetime.fromtimestamp(st.st_mtime, timezone.utc).isoformat(),
        }
        item.update(read_meta(p))
        out.append(item)
        if len(out) >= limit:
            break
    return out


# Basesnake-case-safe PNG names only: no path separators, no dot-dot.
_IMG_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*\.png")


@app.get("/img/{name}")
def image_file(name: str):
    if not _IMG_NAME.fullmatch(name):
        raise HTTPException(404, "not found")
    p = (Path(OUT_DIR) / name).resolve()
    # Belt and braces: the regex already blocks separators, but make sure
    # the resolved path still lands inside OUT_DIR.
    if p.parent != Path(OUT_DIR).resolve() or not p.is_file():
        raise HTTPException(404, "not found")
    return FileResponse(p, media_type="image/png")


@app.get("/", response_class=HTMLResponse)
def gallery():
    return GALLERY_PAGE


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
    # Sidecar metadata so the gallery (and /images) can show the prompt
    # for every image, not just the last one.
    with open(out.with_suffix(".json"), "w") as f:
        json.dump(
            {
                "prompt": prompt,
                "seed": seed,
                "width": w,
                "height": h,
                "steps": steps,
                "seconds": round(dt, 1),
            },
            f,
        )
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
