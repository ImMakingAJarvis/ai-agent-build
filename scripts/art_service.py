#!/usr/bin/env python3
"""jarvis-art — local photo generation service.

Two models:
  * Z-Image-Turbo (6B) — the plain text-to-image model, always loaded.
  * FLUX.2 [klein] 4B  — the reference-capable model: given one or more
    reference images it keeps their subject (a real person) while
    generating the prompt's scene around it. Lazy-loaded on the first
    reference generation (it is a 23 GB model), so a plain photo never
    pays for it.

Serves the models behind a small HTTP API so the rest of the stack
(chat, Discord daemon, dashboard) can ask for photos:

  GET  /          -> the gallery: every generated image, plus a form to
                     send a photo (prompt + size + uploaded reference)
  GET  /health    -> status, models, GPU memory, last generation
  GET  /images    -> generated images, newest first (?limit=N), each with
                     name, prompt, seed and timing (from the sidecar .json
                     written next to each PNG)
  GET  /img/N.png -> a generated image file (basename only, no traversal)
  POST /generate  -> {"prompt": ..., "seed"?: int,
                      "size"?: "1024x1024" | "16x9" | "16:9",
                      "steps"?: int,
                      "model"?: "zimage" | "klein",
                      "refs"?: ["name.png", ...]}
                     saves a PNG under ART_OUT_DIR and returns its path
                     plus timing. size is either explicit pixels (WxH,
                     each dim capped at ART_MAX_DIM) or an aspect ratio
                     (small numbers like 16x9: the long side is scaled to
                     ART_MAX_DIM and both sides rounded to a multiple of
                     16, the VAE patch). refs are file names staged in the
                     refs directory (see /upload, /refs); any refs route
                     the generation to the klein model. Generations take
                     tens of seconds (minutes on klein's first load);
                     clients should allow a few minutes for the request.
  POST /upload    -> raw image bytes (png/jpeg/webp) -> staged as a
                     reference under ART_REFS_DIR; returns {"name": ...}
  GET  /refs/N    -> a staged reference image (basename only, no
                     traversal)

The brain (llama.cpp) owns ~20 GB of the 4090's 24.5 GB and the model is
bigger than the remainder in every component, so the pipeline runs with
enable_sequential_cpu_offload(): each submodule streams to the GPU just
before its forward pass and back to RAM after. Tens of seconds per image
in exchange for near-zero resident VRAM. One generation runs at a time
(409 when busy). Before generating, /generate waits up to ART_VRAM_WAIT_S
(default 90 s) for enough free VRAM (ART_MIN_FREE_MB, default 12 GB — the
DiT burst), because the brain's KV cache usually owns the rest of the GPU
while a session is active; it only returns 503 with a CUDA OOM message if
the headroom never appears (or the brain grabs the GPU mid-generation).

Model weights: ART_MODEL_DIR (default /var/lib/jarvis/art/model) for
Z-Image, ART_KLEIN_DIR (default /var/lib/jarvis/art/models/flux2-klein-4b)
for FLUX.2 [klein], both populated by scripts/install_art.sh /
scripts/install_klein.sh. Images: ART_OUT_DIR (default
/var/lib/jarvis/art/out). References: ART_REFS_DIR (default
/var/lib/jarvis/art/refs) — the jarvis-photo executor also stages job
references here (it runs on the host, which bind-mounts this directory
into the container).
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
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, HTMLResponse

LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "8120"))
MODEL_DIR = os.environ.get("ART_MODEL_DIR", "/var/lib/jarvis/art/model")
OUT_DIR = os.environ.get("ART_OUT_DIR", "/var/lib/jarvis/art/out")
# FLUX.2 [klein] 4B: the reference-capable model (subject-preserving
# editing + text-to-image in one pipeline). Lazy-loaded on first use.
KLEIN_DIR = os.environ.get(
    "ART_KLEIN_DIR", "/var/lib/jarvis/art/models/flux2-klein-4b"
)
# Staged reference images (uploads + per-job refs the executor drops in).
REFS_DIR = os.environ.get("ART_REFS_DIR", "/var/lib/jarvis/art/refs")
# A reference photo is a few MB; cap uploads at Discord's 25 MB.
MAX_REF_BYTES = int(os.environ.get("ART_MAX_REF_MB", "25")) << 20
# Z-Image-Turbo: 9 steps = 8 DiT forwards per the model card; guidance-free
# (distilled), so guidance_scale=0.
DEFAULT_STEPS = int(os.environ.get("ART_STEPS", "9"))
# FLUX.2 [klein]: rectified-flow DiT — the model card runs 4B at 4 steps,
# guidance 1.0.
KLEIN_STEPS = int(os.environ.get("ART_KLEIN_STEPS", "4"))
KLEIN_GUIDANCE = float(os.environ.get("ART_KLEIN_GUIDANCE", "1.0"))
MAX_DIM = int(os.environ.get("ART_MAX_DIM", "1024"))

# A size spec: "1024x1024" (explicit pixels) or "16x9" / "16:9" (aspect).
SIZE_RE = re.compile(r"(\d{1,4})\s*[x:]\s*(\d{1,4})")


def resolve_size(spec):
    """Resolve a size spec to pixel (w, h), each side within [64, MAX_DIM].

    Two forms:
      * aspect ratio — both numbers small (<= 128; "16x9", "16:9", "1x1"):
        long side is scaled to MAX_DIM and both sides are rounded to a
        multiple of 16 (the VAE patch), keeping the ratio as close as the
        grid allows (16x9 -> 1024x576, 4x3 -> 1024x768, 9x16 -> 576x1024);
      * explicit pixels ("1024x576"): used as-is, each side capped at
        MAX_DIM.
    """
    m = SIZE_RE.fullmatch(str(spec).strip())
    if not m:
        raise HTTPException(
            400, "size must look like 1024x1024 or an aspect ratio like 16x9"
        )
    a, b = int(m.group(1)), int(m.group(2))
    if a < 1 or b < 1:
        raise HTTPException(400, "size must look like 1024x1024 or 16x9")
    if a <= 128 and b <= 128:  # small numbers = aspect ratio, not pixels
        scale = MAX_DIM / max(a, b)
        w = int(round(a * scale / 16.0)) * 16
        h = int(round(b * scale / 16.0)) * 16
    else:
        w, h = a, b
    return max(64, min(w, MAX_DIM)), max(64, min(h, MAX_DIM))

# --- VRAM backpressure ----------------------------------------------------
# The brain (llama.cpp) holds most of the 4090's VRAM as KV cache while a
# session is active, and the pipeline needs a transient burst on top of what
# is resident: the DiT transformer alone is ~11.5 GB in bf16. Rather than
# dying with a CUDA OOM the instant the brain is busy, /generate waits for
# enough free VRAM before starting, so a photo requested while the brain is
# active succeeds as soon as the brain's slot goes idle instead of erroring.
# ART_MIN_FREE_MB: free VRAM (MB) required before a generation may start.
# ART_VRAM_WAIT_S: how long to wait for that headroom before giving up (503).
# ART_VRAM_POLL_S: how often to re-check while waiting.
ART_MIN_FREE_MB = int(os.environ.get("ART_MIN_FREE_MB", "12000"))
ART_VRAM_WAIT_S = float(os.environ.get("ART_VRAM_WAIT_S", "90"))
ART_VRAM_POLL_S = float(os.environ.get("ART_VRAM_POLL_S", "2.0"))

pipe = None
_load_error = None
# FLUX.2 [klein]: lazy — loaded only when a reference generation asks for
# it. A separate load lock: the first reference photo loads it while the
# generation lock is already held, and a second concurrent request must
# wait for the load, not start a parallel from_pretrained.
klein_pipe = None
klein_load_error = None
klein_load_lock = threading.Lock()
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


def get_klein():
    """Return the loaded FLUX.2 [klein] pipeline, loading it on first use.

    Raises RuntimeError with a readable message when the weights are
    missing (install_klein.sh) or the load fails, so /generate can turn it
    into a 503. Photo work runs in the brain-swap window (the 4090 is
    free), so the transformer can use plain model CPU offload: only the
    active submodule is on the GPU at a time.
    """
    global klein_pipe, klein_load_error
    if klein_pipe is not None:
        return klein_pipe
    with klein_load_lock:
        if klein_pipe is not None:
            return klein_pipe
        import torch
        from diffusers import Flux2KleinPipeline

        if not (Path(KLEIN_DIR) / "model_index.json").exists():
            klein_load_error = (
                f"klein weights not found in {KLEIN_DIR} — run "
                "scripts/install_klein.sh"
            )
            raise RuntimeError(klein_load_error)
        t0 = time.time()
        klein_pipe = Flux2KleinPipeline.from_pretrained(
            KLEIN_DIR, torch_dtype=torch.bfloat16
        )
        klein_pipe.enable_model_cpu_offload()
        print(
            f"art: klein pipeline loaded in {time.time() - t0:.0f}s", flush=True
        )
        return klein_pipe


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

# The gallery: a small page that fetches /images and renders every
# generated photo with its prompt and timing. It re-fetches every 30 s, so
# a new photo appears without reloading. Timestamps render in the VIEWER'S
# local timezone (moment from a CDN, with a plain-Date fallback when the
# CDN is unreachable) — never raw UTC, which is not the owner's time.
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
  #form { margin: 16px 22px 0; padding: 14px 16px; background: #171a21;
          border: 1px solid #262a33; border-radius: 10px;
          display: grid; gap: 10px; }
  #form label { color: #8b93a7; font-size: 12px; }
  #prompt { width: 100%; box-sizing: border-box; background: #0f1115;
            color: #e6e8ee; border: 1px solid #262a33; border-radius: 8px;
            padding: 8px 10px; font: inherit; resize: vertical; }
  #form .row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
  #size { background: #0f1115; color: #e6e8ee; border: 1px solid #262a33;
          border-radius: 8px; padding: 7px 10px; font: inherit; width: 220px; }
  #file { font-size: 13px; color: #8b93a7; }
  #form button { background: #2b6cb0; color: #fff; border: 0; border-radius: 8px;
                 padding: 8px 16px; font: inherit; font-weight: 600; cursor: pointer; }
  #form button:disabled { opacity: 0.5; cursor: default; }
  #status { color: #8b93a7; font-size: 13px; }
</style>
</head>
<body>
<header><h1>jarvis-art</h1><span id="count"></span></header>
<section id="form">
  <div>
    <label for="prompt">Photo</label>
    <textarea id="prompt" rows="2" placeholder="Describe the photo — or write a few words and hit Assist to expand them"></textarea>
  </div>
  <div class="row">
    <input id="size" placeholder="size (optional, e.g. 16x9)" />
    <label class="file">reference: <input id="file" type="file" accept="image/png,image/jpeg,image/webp"></label>
    <span style="flex: 1"></span>
    <button id="assist" title="Expand the prompt with the thinking mind">Assist</button>
    <button id="send" title="Queue the photo job — the result arrives in your Discord DM">Send</button>
  </div>
  <div class="row"><span id="status"></span></div>
</section>
<div id="grid"></div>
<div id="empty" hidden>No photos yet — send one above, or ask Jarvis on Discord:
<code>!photo a photo of ...</code></div>
<script src="https://cdn.jsdelivr.net/npm/moment@2.30.1/moment.min.js"></script>
<script>
const DASH = location.protocol + '//' + location.hostname + ':8080';
const $ = id => document.getElementById(id);
let busy = false;
function setBusy(b) { busy = b; $('assist').disabled = b; $('send').disabled = b; }
function say(msg) { $('status').textContent = msg; }

// Two-stage prompt: the dashboard expands the basic request with the
// thinking mind (same pipeline as !photoassist) and the result replaces
// the textarea, for review before Send.
async function assist() {
  if (busy) return;
  const request = $('prompt').value.trim();
  if (!request) { say('write a few words first'); return; }
  setBusy(true);
  say('thinking…');
  try {
    const r = await fetch(DASH + '/api/photo/assist', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ request, size: $('size').value.trim() }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) { say(d.error || d.detail || ('assist failed: ' + r.status)); return; }
    $('prompt').value = d.prompt;
    say(d.note || 'prompt expanded — hit Send when it looks right');
  } catch (e) {
    say('assist failed: ' + e);
  }
  setBusy(false);
}

// Queue the photo: an uploaded reference is staged on the art service
// first (POST /upload, same origin), then the dashboard queues the
// photo-tagged job with it as the reference (the result keeps the
// subject from the photo).
async function send() {
  if (busy) return;
  const prompt = $('prompt').value.trim();
  if (!prompt) { say('write a prompt first'); return; }
  setBusy(true);
  try {
    let ref = '';
    const file = $('file').files[0];
    if (file) {
      say('uploading reference…');
      const r = await fetch('/upload', {
        method: 'POST',
        headers: file.type ? { 'Content-Type': file.type } : {},
        body: file,
      });
      const d = await r.json().catch(() => ({}));
      if (!r.ok) { say(d.detail || d.error || ('upload failed: ' + r.status)); return; }
      ref = d.name;
    }
    say('queuing photo…');
    const r = await fetch(DASH + '/api/photo/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ prompt, size: $('size').value.trim(), ref }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) { say(d.error || d.detail || ('queue failed: ' + r.status)); return; }
    say('queued ' + d.id + ' — it runs in the next photo window; the result lands in your Discord DM');
    $('prompt').value = '';
    $('size').value = '';
    $('file').value = '';
  } catch (e) {
    say('queue failed: ' + e);
  }
  setBusy(false);
}
$('assist').onclick = assist;
$('send').onclick = send;

// Timestamps: moment in the viewer's local zone (e.g. "2026-11-08 05:36 PST");
// if the CDN never loaded, fall back to the browser's own local formatting.
function fmtWhen(iso) {
  if (!iso) return null;
  if (window.moment) return moment(iso).local().format('YYYY-MM-DD HH:mm z');
  return new Date(iso).toLocaleString([], {dateStyle: 'medium', timeStyle: 'short'});
}
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
        fmtWhen(it.modified),
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


def free_vram_mb():
    """Free VRAM on the GPU in MB, or None when CUDA is unavailable."""
    try:
        import torch

        if torch.cuda.is_available():
            free, _total = torch.cuda.mem_get_info()
            return round(free / 2**20)
    except Exception:
        pass
    return None


def _vram_oom(what):
    """503 for a CUDA OOM mid-generation: the brain took the GPU VRAM
    while the generation was running."""
    free = free_vram_mb()
    return HTTPException(
        503,
        "CUDA OOM mid-generation — the brain took the GPU VRAM while the "
        f"{what} was running ({free if free is not None else '?'} MB free); "
        "try again later",
    )


def wait_for_vram(min_free_mb, timeout_s, poll_s, free_mb_fn=None, clear_cache_fn=None):
    """Block until at least min_free_mb of VRAM is free, up to timeout_s.

    Polls free_mb_fn (default: the live GPU) every poll_s seconds, calling
    clear_cache_fn (default: torch.cuda.empty_cache) between polls to release
    orphaned blocks the brain may have dropped. Returns True when enough VRAM
    is free — or immediately when there is no GPU to contend over — and False
    when the timeout expires first. A final free check runs at the deadline
    so a slot that idles out at the last moment still wins.
    """
    if free_mb_fn is None:
        free_mb_fn = free_vram_mb
    if clear_cache_fn is None:
        def clear_cache_fn():
            try:
                import torch

                if torch.cuda.is_available():
                    torch.cuda.empty_cache()
            except Exception:
                pass

    deadline = time.time() + timeout_s
    while True:
        free = free_mb_fn()
        if free is None or free >= min_free_mb:
            return True
        if time.time() >= deadline:
            return False
        clear_cache_fn()
        time.sleep(poll_s)


@app.get("/health")
def health():
    return {
        "status": "ok" if pipe is not None else "no-model",
        "model": "z-image-turbo",
        "loaded": pipe is not None,
        "error": _load_error,
        # Reference model: weights on disk, pipeline loaded (lazy).
        "klein_weights": (Path(KLEIN_DIR) / "model_index.json").exists(),
        "klein_loaded": klein_pipe is not None,
        "klein_error": klein_load_error,
        "busy": _busy["since"] is not None,
        "gpu": gpu_stats(),
        "free_vram_mb": free_vram_mb(),
        "min_free_mb": ART_MIN_FREE_MB,
        "vram_wait_s": ART_VRAM_WAIT_S,
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
    model, refs = _pick_model(payload)
    if model == "klein":
        # Validate the refs now (before taking the lock) so a bad name is a
        # fast 400, not a 10-minute load + 400.
        _stage_check(refs)
    if not _lock.acquire(blocking=False):
        raise HTTPException(409, "a generation is already running")
    _busy["since"] = time.time()
    try:
        return _run(prompt, payload, model, refs)
    finally:
        _lock.release()
        _busy["since"] = None


def _pick_model(payload):
    """Resolve (model, refs) from a /generate payload.

    refs imply the reference-capable model (klein): an uploaded photo is
    meaningless to plain text-to-image. An explicit "zimage" with refs is
    still routed to klein (the refs win — the caller attached a photo for a
    reason); an unknown model name is a 400.
    """
    model = (payload.get("model") or "zimage").strip().lower()
    if model not in ("zimage", "klein"):
        raise HTTPException(400, "model must be 'zimage' or 'klein'")
    refs = [str(r) for r in (payload.get("refs") or [])]
    if refs:
        model = "klein"
    return model, refs


# Reference names are staged files: alnum plus dot/dash/underscore, one
# extension, no separators or dot-dot (same rule as /img names).
_REF_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*\.(?:png|jpe?g|webp)")


def _stage_check(refs):
    """Resolve ref names to files in REFS_DIR; 400 on any bad/missing name."""
    for name in refs:
        if not _REF_NAME.fullmatch(name):
            raise HTTPException(400, f"bad ref name {name!r}")
        p = (Path(REFS_DIR) / name).resolve()
        if p.parent != Path(REFS_DIR).resolve() or not p.is_file():
            raise HTTPException(400, f"no such reference: {name}")


@app.post("/upload")
async def upload(request: Request):
    """Stage one uploaded image as a reference and return its name.

    Raw image bytes (png/jpeg/webp) in the body — the gallery form's file
    input posts it directly. The name is generated here, so a re-upload
    never clobbers a staged job reference.
    """
    import uuid

    ctype = (request.headers.get("content-type") or "").split(";")[0].strip()
    ext = {
        "image/png": ".png",
        "image/jpeg": ".jpg",
        "image/webp": ".webp",
    }.get(ctype, ".png")
    body = await request.body()
    if not body:
        raise HTTPException(400, "empty upload")
    if len(body) > MAX_REF_BYTES:
        raise HTTPException(
            413, f"upload exceeds {MAX_REF_BYTES >> 20} MB"
        )
    Path(REFS_DIR).mkdir(parents=True, exist_ok=True)
    name = f"upload-{int(time.time())}-{uuid.uuid4().hex[:6]}{ext}"
    (Path(REFS_DIR) / name).write_bytes(body)
    return {"name": name}


@app.get("/refs/{name}")
def ref_file(name: str):
    """Serve one staged reference image (basename only, no traversal)."""
    if not _REF_NAME.fullmatch(name):
        raise HTTPException(404, "not found")
    p = (Path(REFS_DIR) / name).resolve()
    if p.parent != Path(REFS_DIR).resolve() or not p.is_file():
        raise HTTPException(404, "not found")
    media = {".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
             ".webp": "image/webp"}.get(p.suffix.lower(), "application/octet-stream")
    return FileResponse(p, media_type=media)


def _run(prompt, payload, model, refs):
    import torch

    w, h = resolve_size(payload.get("size") or "1024x1024")
    if model == "klein":
        steps = int(payload.get("steps") or KLEIN_STEPS)
    else:
        steps = int(payload.get("steps") or DEFAULT_STEPS)
    seed = (
        int(payload["seed"])
        if payload.get("seed") is not None
        else int(torch.randint(0, 2**31, (1,)).item())
    )
    gen = torch.Generator(device="cpu").manual_seed(seed)

    # VRAM backpressure: the brain usually owns most of the GPU. Wait for
    # enough free VRAM for the DiT burst before starting, so a photo asked
    # while the brain is busy succeeds as soon as it goes idle.
    if not wait_for_vram(ART_MIN_FREE_MB, ART_VRAM_WAIT_S, ART_VRAM_POLL_S):
        free = free_vram_mb()
        raise HTTPException(
            503,
            "not enough free VRAM for a photo: "
            f"{free if free is not None else '?'} MB free, "
            f"{ART_MIN_FREE_MB} MB needed (waited {ART_VRAM_WAIT_S:.0f}s "
            "for the brain to go idle); try again later",
        )

    t0 = time.time()
    if model == "klein":
        # Reference generation: klein's pipeline takes the reference
        # image(s) as the first argument — a single PIL image or a list —
        # and keeps their subject while rendering the prompt's scene.
        try:
            klein = get_klein()
        except RuntimeError as e:
            raise HTTPException(503, str(e))
        from PIL import Image as PILImage

        ref_paths = [(Path(REFS_DIR) / r) for r in refs]
        ref_images = [PILImage.open(p).convert("RGB") for p in ref_paths]
        image = ref_images if ref_images else None
        try:
            img = klein(
                image=image,
                prompt=prompt,
                width=w,
                height=h,
                num_inference_steps=steps,
                guidance_scale=KLEIN_GUIDANCE,
                generator=gen,
            ).images[0]
        except torch.OutOfMemoryError:
            raise _vram_oom("photo")
    else:
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
            raise _vram_oom("photo")
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
