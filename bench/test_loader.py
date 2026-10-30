#!/usr/bin/env python3
"""test_loader.py — tests for bench/config.py (the YAML -> SQLite loader).

Run:  python3 test_loader.py        (host or inside the jarvis/art image)
Exit: 0 when all tests pass, 1 otherwise.

Covers the job's loader acceptance:
  1. valid sample YAML  -> rows upserted with the expected values
  2. re-run with a changed value -> same row updated, no duplicate
  3. bad YAML (syntax error) -> clear error naming the file, no exception
  4. bad shape (missing required key) -> clear error, other file still loads
"""

import json
import sqlite3
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import config  # noqa: E402

SCHEMA = (Path(__file__).resolve().parent / "schema.sql").read_text()

FAILURES = []


def check(name, cond, detail=""):
    print(f"{'PASS' if cond else 'FAIL'}: {name}" + (f"  [{detail}]" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def fresh_db(tmp: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(tmp / "bench.db"))
    conn.executescript(SCHEMA)
    conn.commit()
    return conn


def write_config(tmp: Path, models: str, prompts: str):
    (tmp / config.MODELS_FILE).write_text(models)
    (tmp / config.PROMPTS_FILE).write_text(prompts)


GOOD_MODELS = """
models:
  - model_id: m1
    repo: org/m1
    pipeline_class: TestPipeline
    quant: fp8
    steps: 4
    guidance: 0
    caps: [t2i, i2i]
    size_bytes: 1000
    status: known
"""

GOOD_PROMPTS = """
prompts:
  - id: p1
    title: One
    category: t2i
    prompt_text: "a red door"
    caps: [t2i]
  - id: p2
    title: Two
    category: t2i
    prompt_text: "a blue wall"
"""


def main() -> int:
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        conn = fresh_db(tmp)

        # 1. valid sample YAML -> rows upserted
        write_config(tmp, GOOD_MODELS, GOOD_PROMPTS)
        r = config.load_config(conn, tmp)
        check("valid: no errors", r["errors"] == [], str(r["errors"]))
        check("valid: counts", (r["models"], r["prompts"]) == (1, 2), str(r))
        row = conn.execute("SELECT repo, steps, caps FROM models WHERE model_id='m1'").fetchone()
        check("valid: model row values",
              row == ("org/m1", 4, json.dumps(["t2i", "i2i"])), str(row))
        prow = conn.execute("SELECT title, prompt_text FROM prompts WHERE id='p1'").fetchone()
        check("valid: prompt row values", prow == ("One", "a red door"), str(prow))

        # 2. upsert: changed value updates the same row, no duplicate
        write_config(tmp, GOOD_MODELS.replace("org/m1", "org/m1-v2")
                     .replace("steps: 4", "steps: 8"), GOOD_PROMPTS)
        r = config.load_config(conn, tmp)
        check("upsert: no errors", r["errors"] == [], str(r["errors"]))
        n = conn.execute("SELECT COUNT(*) FROM models").fetchone()[0]
        check("upsert: still one model row", n == 1, f"count={n}")
        row = conn.execute("SELECT repo, steps FROM models WHERE model_id='m1'").fetchone()
        check("upsert: values updated", row == ("org/m1-v2", 8), str(row))

        # 3. bad YAML (syntax) -> clear error, no exception, service can start
        write_config(tmp, "models:\n  - model_id: [unclosed\n    oops:", GOOD_PROMPTS)
        try:
            r = config.load_config(conn, tmp)
            check("bad yaml: no exception raised", True)
        except Exception as e:
            check("bad yaml: no exception raised", False, repr(e))
            r = {"errors": [repr(e)]}
        check("bad yaml: clear error names the file",
              any("models.yaml" in e for e in r["errors"]), str(r["errors"]))
        # the good prompts file must still have loaded
        check("bad yaml: other file still loads", r["prompts"] == 2, str(r))

        # 4. bad shape: missing required key -> clear error, good entry kept
        write_config(tmp, "models:\n  - repo: org/missing-id\n    pipeline_class: X\n",
                     GOOD_PROMPTS)
        r = config.load_config(conn, tmp)
        check("bad shape: no exception raised", True)
        check("bad shape: clear error",
              any("missing required" in e for e in r["errors"]), str(r["errors"]))
        conn.close()

    print(f"\n{len(FAILURES)} failure(s)" if FAILURES else "\nall tests passed")
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
