-- jarvis-bench SQLite schema (bench.db, created at startup if missing).
--
-- Tables:
--   models       candidate t2i models from bench/models.yaml (upserted)
--   prompts      the fixed prompt set from bench/prompts.yaml (upserted)
--   runs         one row per bench run (all prompts x a set of models)
--   generations  one row per (run, prompt, model) image attempt + metrics
--   votes        blind Tinder-swipe votes on generations (like = keep)
--
-- `like` is a SQLite keyword: quote it ("like") in DML, unquoted is fine
-- in the DDL below.

CREATE TABLE IF NOT EXISTS models (
    model_id       TEXT PRIMARY KEY,          -- upsert key, used in generations
    repo           TEXT NOT NULL,             -- Hugging Face repo id
    pipeline_class TEXT NOT NULL,             -- diffusers pipeline class name
    quant          TEXT,                      -- bf16 | fp8 | q4 ...
    steps          INTEGER,                   -- sampling steps
    guidance       REAL,                      -- guidance scale (0 = distilled)
    caps           TEXT,                      -- JSON list, e.g. ["t2i","i2i","ref"]
    size_bytes     INTEGER,                   -- weight size on disk (0 = unknown)
    status         TEXT NOT NULL DEFAULT 'known'  -- known|downloading|ready|error
);

CREATE TABLE IF NOT EXISTS prompts (
    id          TEXT PRIMARY KEY,             -- upsert key, used in generations
    title       TEXT NOT NULL,
    category    TEXT,                         -- t2i | img2img | ref | consistency ...
    prompt_text TEXT NOT NULL,
    caps        TEXT,                         -- JSON list of required capabilities
    images      TEXT,                         -- JSON list of input image file names
    asset_path  TEXT                          -- path (relative to bench/imgs) of the asset
);

CREATE TABLE IF NOT EXISTS runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at  TEXT,                         -- ISO 8601 UTC
    finished_at TEXT,
    status      TEXT NOT NULL DEFAULT 'running',  -- running|done|error
    note        TEXT
);

CREATE TABLE IF NOT EXISTS generations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER REFERENCES runs(id),
    prompt_id     TEXT    REFERENCES prompts(id),
    model_id      TEXT    REFERENCES models(model_id),
    img_index     INTEGER,                    -- which image of a multi-image prompt
    seed          INTEGER,
    size          TEXT,                       -- e.g. "1024x1024"
    status        TEXT NOT NULL DEFAULT 'pending',  -- pending|running|done|error
    img_path      TEXT,                       -- absolute path of the saved PNG
    wall_s        REAL,                       -- end-to-end seconds for this image
    its           REAL,                       -- iterations (steps) per second
    peak_vram_mib INTEGER,                    -- peak GPU memory during generation
    gpu_wh        REAL,                       -- average GPU power draw (W)
    rss_mib       INTEGER,                    -- process RSS at the end (MiB)
    error         TEXT
);

CREATE TABLE IF NOT EXISTS votes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id INTEGER NOT NULL REFERENCES generations(id),
    "like"        INTEGER NOT NULL,           -- 1 = keep, 0 = pass
    created_at    TEXT NOT NULL               -- ISO 8601 UTC
);

CREATE INDEX IF NOT EXISTS idx_generations_run   ON generations(run_id);
CREATE INDEX IF NOT EXISTS idx_generations_model ON generations(model_id);
CREATE INDEX IF NOT EXISTS idx_votes_generation  ON votes(generation_id);
