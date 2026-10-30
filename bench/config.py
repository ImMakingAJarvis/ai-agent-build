"""bench/config.py — config loader for jarvis-bench.

Reads bench/models.yaml and bench/prompts.yaml (from BENCH_CONFIG_DIR,
default: this directory) and upserts them into the models/prompts tables.

Contract:
  - load_config() never raises for bad input files: it returns
    {"models": n, "prompts": n, "errors": [str, ...]} so the app can
    start and report a clear error via /api/status.
  - upserts are keyed on model_id / id; a YAML re-write updates the row,
    it never duplicates. Rows absent from the YAML are left untouched
    (later jobs manage model status, e.g. downloads).
"""

import json
import os
import re
import sqlite3
from pathlib import Path

import yaml

MODELS_FILE = "models.yaml"
PROMPTS_FILE = "prompts.yaml"

# Required keys per entry; missing/empty -> clear error naming the entry.
MODEL_REQUIRED = ("model_id", "repo", "pipeline_class")
PROMPT_REQUIRED = ("id", "title", "prompt_text")


def _config_dir() -> Path:
    return Path(os.environ.get("BENCH_CONFIG_DIR", Path(__file__).resolve().parent))


def _read_yaml_list(path: Path, key: str):
    """Return (items, error). error is None on success."""
    if not path.is_file():
        return None, f"{path.name} not found at {path}"
    try:
        data = yaml.safe_load(path.read_text())
    except yaml.YAMLError as e:
        mark = getattr(e, "problem_mark", None)
        where = f" line {mark.line + 1}" if mark is not None else ""
        return None, f"bad YAML in {path.name}{where}: {getattr(e, 'problem', e)}"
    if data is None:
        return [], None  # empty file: nothing to upsert, not an error
    if not isinstance(data, dict) or not isinstance(data.get(key), list):
        return None, (f"bad shape in {path.name}: top level must be a mapping "
                      f"with a '{key}' list")
    return data[key], None


def _coerce_list(value) -> str:
    """caps/images: accept YAML list or JSON string, store as JSON text."""
    if value is None:
        return "[]"
    if isinstance(value, str):
        value = json.loads(value)  # may raise -> caller reports it
    return json.dumps(list(value))


def upsert_models(conn: sqlite3.Connection, models: list) -> tuple[int, list[str]]:
    """Upsert model entries. Returns (count, errors)."""
    errors = []
    n = 0
    for i, m in enumerate(models):
        if not isinstance(m, dict):
            errors.append(f"models.yaml entry #{i}: not a mapping ({m!r})")
            continue
        missing = [k for k in MODEL_REQUIRED if not m.get(k)]
        if missing:
            errors.append(f"models.yaml entry #{i}: missing required key(s) {missing}")
            continue
        try:
            conn.execute(
                """INSERT INTO models (model_id, repo, pipeline_class, quant, steps,
                                       guidance, caps, size_bytes, status)
                   VALUES (?,?,?,?,?,?,?,?,?)
                   ON CONFLICT(model_id) DO UPDATE SET
                     repo=excluded.repo, pipeline_class=excluded.pipeline_class,
                     quant=excluded.quant, steps=excluded.steps,
                     guidance=excluded.guidance, caps=excluded.caps,
                     size_bytes=excluded.size_bytes, status=excluded.status""",
                (str(m["model_id"]), str(m["repo"]), str(m["pipeline_class"]),
                 m.get("quant"), m.get("steps"), m.get("guidance"),
                 _coerce_list(m.get("caps")), m.get("size_bytes") or 0,
                 m.get("status") or "known"),
            )
        except (ValueError, TypeError, json.JSONDecodeError) as e:
            errors.append(f"models.yaml entry {m.get('model_id')}: {e}")
            continue
        n += 1
    conn.commit()
    return n, errors


def upsert_prompts(conn: sqlite3.Connection, prompts: list) -> tuple[int, list[str]]:
    """Upsert prompt entries. Returns (count, errors)."""
    errors = []
    n = 0
    for i, p in enumerate(prompts):
        if not isinstance(p, dict):
            errors.append(f"prompts.yaml entry #{i}: not a mapping ({p!r})")
            continue
        missing = [k for k in PROMPT_REQUIRED if not p.get(k)]
        if missing:
            errors.append(f"prompts.yaml entry #{i}: missing required key(s) {missing}")
            continue
        try:
            conn.execute(
                """INSERT INTO prompts (id, title, category, prompt_text, caps, images,
                                        asset_path)
                   VALUES (?,?,?,?,?,?,?)
                   ON CONFLICT(id) DO UPDATE SET
                     title=excluded.title, category=excluded.category,
                     prompt_text=excluded.prompt_text, caps=excluded.caps,
                     images=excluded.images, asset_path=excluded.asset_path""",
                (str(p["id"]), str(p["title"]), p.get("category"), str(p["prompt_text"]),
                 _coerce_list(p.get("caps")), _coerce_list(p.get("images")),
                 p.get("asset_path")),
            )
        except (ValueError, TypeError, json.JSONDecodeError) as e:
            errors.append(f"prompts.yaml entry {p.get('id')}: {e}")
            continue
        n += 1
    conn.commit()
    return n, errors


def load_config(conn: sqlite3.Connection, config_dir: Path | None = None) -> dict:
    """Load both YAML files into the DB. Never raises; errors are collected."""
    cfg_dir = Path(config_dir) if config_dir else _config_dir()
    result = {"models": 0, "prompts": 0, "errors": [],
              "models_file": str(cfg_dir / MODELS_FILE),
              "prompts_file": str(cfg_dir / PROMPTS_FILE)}

    models, err = _read_yaml_list(cfg_dir / MODELS_FILE, "models")
    if err:
        result["errors"].append(err)
    else:
        n, errs = upsert_models(conn, models)
        result["models"] = n
        result["errors"].extend(errs)

    prompts, err = _read_yaml_list(cfg_dir / PROMPTS_FILE, "prompts")
    if err:
        result["errors"].append(err)
    else:
        n, errs = upsert_prompts(conn, prompts)
        result["prompts"] = n
        result["errors"].extend(errs)

    return result
