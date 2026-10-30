"""bench/app.py — jarvis-bench service (skeleton).

T2I model benchmark harness: candidate local t2i models (bench/models.yaml)
run over a fixed prompt set (bench/prompts.yaml), blind swipe voting,
per-model metrics. This revision is the skeleton only — compose wiring,
health/status API, SQLite schema, config loader. No generation yet
(later jobs of plan plan-jarvis-model-downloader-1793259146).

Endpoints:
  GET /health     -> 200 {status, service, gpu free VRAM, config} when active;
                     503 {status: disabled} while the kill switch file exists
  GET /api/status -> model/prompt counts, last run state, config load errors

Kill switch (the box's standard boot inhibit, like dashboard.disabled /
agent-run.disabled): touch /var/lib/jarvis/bench.disabled -> the service
idles at boot (no GPU, API 503) until the file is removed; it re-enables
itself within ~30 s of removal without a container restart.

DB:  ~/.local/share/jarvis/bench/bench.db (SQLite, schema created at startup)
Data: /var/lib/jarvis/bench/{imgs,models}  (images / candidate weights)
"""

import asyncio
import os
import sqlite3
import subprocess
import threading
from contextlib import asynccontextmanager
from pathlib import Path

import uvicorn
from fastapi import FastAPI, HTTPException

import config

HOST = os.environ.get("BENCH_HOST", "0.0.0.0")
PORT = int(os.environ.get("BENCH_PORT", "8121"))
DATA_DIR = Path(os.environ.get("BENCH_DATA_DIR",
                               str(Path.home() / ".local" / "share" / "jarvis" / "bench")))
IMG_DIR = Path(os.environ.get("BENCH_IMG_DIR", "/var/lib/jarvis/bench/imgs"))
MODEL_DIR = Path(os.environ.get("BENCH_MODEL_DIR", "/var/lib/jarvis/bench/models"))
KILL_SWITCH = Path(os.environ.get("BENCH_KILL_SWITCH_FILE",
                                  "/var/lib/jarvis/bench.disabled"))
DB_PATH = DATA_DIR / "bench.db"
SCHEMA_PATH = Path(__file__).resolve().parent / "schema.sql"
WATCH_INTERVAL_S = 30  # how soon a removed kill switch re-enables the service

# Shared service state (single process, few requests: one connection + lock).
conn: sqlite3.Connection | None = None
_lock = threading.Lock()
state = {"disabled": False, "config": None}


def is_disabled() -> bool:
    return KILL_SWITCH.is_file()


def init_db() -> None:
    global conn
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    IMG_DIR.mkdir(parents=True, exist_ok=True)
    MODEL_DIR.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(DB_PATH), check_same_thread=False)
    with _lock:
        conn.executescript(SCHEMA_PATH.read_text())
        conn.commit()
    print(f"bench: db ready at {DB_PATH}", flush=True)


def gpu_free_vram():
    """Free/total VRAM in MiB. nvidia-smi first (no CUDA context cost),
    torch.cuda.mem_get_info as fallback (same as the art service)."""
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=memory.free,memory.total",
             "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10)
        if out.returncode == 0:
            free, total = out.stdout.strip().splitlines()[0].split(", ")
            return {"free_mib": int(free), "total_mib": int(total)}
    except Exception:
        pass
    try:
        import torch
        if torch.cuda.is_available():
            free, total = torch.cuda.mem_get_info()
            return {"free_mib": round(free / 2**20), "total_mib": round(total / 2**20)}
    except Exception:
        pass
    return None


def load_now() -> None:
    """(Re)load the YAML config into the DB. Errors are collected, not raised."""
    with _lock:
        state["config"] = config.load_config(conn)
    for err in state["config"]["errors"]:
        print(f"bench: CONFIG ERROR: {err}", flush=True)
    print(f"bench: config loaded: {state['config']['models']} models, "
          f"{state['config']['prompts']} prompts "
          f"({len(state['config']['errors'])} error(s))", flush=True)


async def watch_kill_switch() -> None:
    """Re-enable within WATCH_INTERVAL_S of the kill switch file being
    removed. A switch appearing mid-run takes effect at the next restart
    (we never kill a running bench)."""
    while True:
        await asyncio.sleep(WATCH_INTERVAL_S)
        try:
            if is_disabled() and not state["disabled"]:
                print("bench: kill switch appeared; takes effect on next "
                      "start/restart", flush=True)
            elif not is_disabled() and state["disabled"]:
                await asyncio.to_thread(load_now)
                state["disabled"] = False
                print("bench: kill switch removed -> enabled", flush=True)
        except Exception as e:
            print(f"bench: kill-switch watch error: {e}", flush=True)


@asynccontextmanager
async def lifespan(app: FastAPI):
    init_db()
    if is_disabled():
        state["disabled"] = True
        print(f"bench: kill switch {KILL_SWITCH} present -> idling "
              f"(no GPU load, API disabled)", flush=True)
    else:
        load_now()
    watch = asyncio.create_task(watch_kill_switch())
    yield
    watch.cancel()
    with _lock:
        conn.close()


app = FastAPI(title="jarvis-bench", lifespan=lifespan)


@app.get("/")
def root():
    return {"service": "jarvis-bench",
            "endpoints": ["/health", "/api/status"]}


@app.get("/health")
def health():
    if state["disabled"]:
        raise HTTPException(status_code=503, detail={
            "status": "disabled", "service": "jarvis-bench",
            "reason": (f"kill switch file {KILL_SWITCH} present; "
                       f"rm it to re-enable (no restart needed)")})
    cfg = state["config"] or {}
    return {
        "status": "ok",
        "service": "jarvis-bench",
        "gpu": gpu_free_vram(),
        "config": {"models": cfg.get("models", 0),
                   "prompts": cfg.get("prompts", 0),
                   "errors": cfg.get("errors", [])},
    }


@app.get("/api/status")
def api_status():
    with _lock:
        n_models = conn.execute("SELECT COUNT(*) FROM models").fetchone()[0]
        n_prompts = conn.execute("SELECT COUNT(*) FROM prompts").fetchone()[0]
        models_by_status = dict(
            conn.execute("SELECT status, COUNT(*) FROM models "
                         "GROUP BY status").fetchall())
        last_run = conn.execute(
            "SELECT id, started_at, finished_at, status, note FROM runs "
            "ORDER BY id DESC LIMIT 1").fetchone()
        cfg = dict(state["config"] or {})
    return {
        "service": "jarvis-bench",
        "disabled": state["disabled"],
        "models": n_models,
        "prompts": n_prompts,
        "models_by_status": models_by_status,
        "last_run": (dict(zip(("id", "started_at", "finished_at", "status",
                               "note"), last_run)) if last_run else None),
        "config": cfg,
        "db": str(DB_PATH),
    }


if __name__ == "__main__":
    uvicorn.run(app, host=HOST, port=PORT, log_level="info")
