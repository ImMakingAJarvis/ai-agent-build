"""Jarvis Memory Service — a local vector database for work logs and notes.

Indexes the logs and outputs the agent produces (job transcripts, job specs,
state/summaries, kanban board, pending queue, repo READMEs and docs) and
answers semantic search queries over them, so the chatbot can reference
what work happened and what is up for scheduling next.

Fully local:
- embeddings: all-MiniLM-L6-v2 (ONNX via onnxruntime, CPU, ~5ms/chunk)
- storage: SQLite at ~/.local/share/jarvis/memory/memory.db

Endpoints:
  GET  /health   service + model status
  GET  /stats    chunk/doc counts, last reindex
  POST /index    upsert one doc: {source, path, title, text, meta?}
  POST /search   {query, k?, source?} -> top-k chunks with metadata
  POST /reindex  re-scan every source
  POST /purge    {source} -> drop everything indexed from that source

Also runs as a CLI:
  python3 memory_service.py --reindex   re-scan every source, then exit
  python3 memory_service.py --search "what did we do about wake word"
  python3 memory_service.py --stats
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import sqlite3
import struct
import sys
import time
from datetime import datetime, timezone

import numpy as np
import onnxruntime as ort
import tokenizers
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
import uvicorn

log = logging.getLogger("jarvis-memory")

DATA_DIR = os.environ.get("JARVIS_DATA_DIR") or os.path.expanduser("~/.local/share/jarvis")
MODEL_DIR = os.environ.get("MEMORY_MODEL_DIR") or os.path.join(DATA_DIR, "memory", "model")
DB_PATH = os.path.join(DATA_DIR, "memory", "memory.db")
REPO_DIR = os.environ.get("JARVIS_REPO_DIR") or "/home/agent/ai-agent-build"
DASHBOARD_URL = os.environ.get("JARVIS_DASHBOARD_URL") or "http://127.0.0.1:8080"

CHUNK_MAX = 450      # chars per chunk (~110 tokens for MiniLM)
CHUNK_OVERLAP = 60   # chars repeated between adjacent chunks
FILE_CAP = 1 << 20   # skip files larger than 1 MB
DOC_CAP = 256 << 10  # repo docs: 256 KB


# --- Embeddings -------------------------------------------------------------

class Embedder:
    """all-MiniLM-L6-v2 via ONNX: tokenize -> run -> masked mean pool -> L2."""

    def __init__(self, model_dir: str):
        self.tokenizer = tokenizers.Tokenizer.from_file(os.path.join(model_dir, "tokenizer.json"))
        self.tokenizer.no_truncation()
        self.session = ort.InferenceSession(
            os.path.join(model_dir, "model.onnx"),
            providers=["CPUExecutionProvider"],
        )
        self.dim = self.session.get_outputs()[0].shape[-1]

    def embed(self, texts: list[str]) -> np.ndarray:
        if not texts:
            return np.zeros((0, self.dim), dtype=np.float32)
        encs = self.tokenizer.encode_batch(texts)
        L = max(len(e.ids) for e in encs)
        ids = np.zeros((len(encs), L), dtype=np.int64)
        mask = np.zeros_like(ids)
        for i, e in enumerate(encs):
            ids[i, : len(e.ids)] = e.ids
            mask[i, : len(e.ids)] = e.attention_mask
        tt = np.zeros_like(ids)
        h = self.session.run(None, {
            "input_ids": ids,
            "attention_mask": mask,
            "token_type_ids": tt,
        })[0]
        v = (h * mask[:, :, None]).sum(axis=1) / mask.sum(axis=1)[:, None]
        v /= np.linalg.norm(v, axis=1, keepdims=True) + 1e-12
        return v.astype(np.float32)


# --- Chunking ---------------------------------------------------------------

def chunk_text(text: str, max_chars: int = CHUNK_MAX, overlap: int = CHUNK_OVERLAP) -> list[str]:
    """Split text into overlapping chunks, packing paragraphs where possible."""
    text = text.strip()
    if not text:
        return []
    chunks: list[str] = []
    buf = ""
    for para in text.split("\n"):
        para = para.strip()
        if not para:
            continue
        # Hard-split oversized paragraphs.
        while len(para) > max_chars:
            if buf:
                chunks.append(buf)
                buf = buf[-overlap:]
            buf = buf + para[: max_chars - len(buf)] if len(buf) < max_chars else para[:max_chars]
            para = para[max_chars - overlap:] if overlap else ""
            if len(para) <= overlap:
                break
        if not buf:
            buf = para
        elif len(buf) + len(para) + 1 <= max_chars:
            buf = buf + "\n" + para
        else:
            chunks.append(buf)
            buf = para[-overlap:] + "\n" + para if len(para) > overlap else para
    if buf.strip():
        chunks.append(buf)
    # De-dup: overlapping tails can produce identical short chunks.
    out, seen = [], set()
    for c in chunks:
        c = c.strip()
        if c and c not in seen:
            seen.add(c)
            out.append(c)
    return out


# --- Storage ----------------------------------------------------------------

SCHEMA = """
CREATE TABLE IF NOT EXISTS docs(
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,
  path   TEXT NOT NULL,
  title  TEXT NOT NULL DEFAULT '',
  hash   TEXT NOT NULL,
  meta   TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL,
  UNIQUE(source, path)
);
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY,
  doc_id INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  embedding BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id);
CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT);
"""


def doc_hash(source: str, path: str, text: str) -> str:
    return hashlib.sha1(f"{source}|{path}|{text}".encode()).hexdigest()


class MemoryStore:
    def __init__(self, db_path: str):
        os.makedirs(os.path.dirname(db_path), exist_ok=True)
        self.db = sqlite3.connect(db_path, check_same_thread=False)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA foreign_keys=ON")
        self.db.executescript(SCHEMA)
        self.db.commit()
        # Cached vector matrix for fast search; invalidated on writes.
        self._vec_cache: np.ndarray | None = None
        self._id_cache: list[int] = []

    def _invalidate(self):
        self._vec_cache = None

    def upsert_doc(self, source: str, path: str, title: str, text: str,
                   meta: dict, embedder: Embedder) -> int:
        h = doc_hash(source, path, text)
        row = self.db.execute(
            "SELECT id, hash FROM docs WHERE source=? AND path=?", (source, path)
        ).fetchone()
        if row and row[1] == h:
            return 0  # unchanged
        if row:
            self.db.execute("DELETE FROM chunks WHERE doc_id=?", (row[0],))
        now = datetime.now(timezone.utc).isoformat(timespec="seconds")
        if row:
            self.db.execute(
                "UPDATE docs SET hash=?, title=?, meta=?, updated_at=? WHERE id=?",
                (h, title, json.dumps(meta), now, row[0]),
            )
            doc_id = row[0]
        else:
            cur = self.db.execute(
                "INSERT INTO docs(source, path, title, hash, meta, updated_at) VALUES(?,?,?,?,?,?)",
                (source, path, title, h, json.dumps(meta), now),
            )
            doc_id = cur.lastrowid
        n = 0
        for i, chunk in enumerate(chunk_text(text)):
            v = embedder.embed([chunk])[0]
            self.db.execute(
                "INSERT INTO chunks(doc_id, text, embedding) VALUES(?,?,?)",
                (doc_id, chunk, struct.pack(f"{len(v)}f", *v)),
            )
            n += 1
        self.db.commit()
        self._invalidate()
        return n

    def delete_source(self, source: str) -> int:
        ids = [r[0] for r in self.db.execute("SELECT id FROM docs WHERE source=?", (source,))]
        for i in ids:
            self.db.execute("DELETE FROM chunks WHERE doc_id=?", (i,))
        self.db.execute("DELETE FROM docs WHERE source=?", (source,))
        self.db.commit()
        self._invalidate()
        return len(ids)

    def forget_missing(self, source: str, keep: set[tuple[str, str, str]]):
        """Drop docs of this source whose (path, hash) are not in keep."""
        rows = self.db.execute(
            "SELECT id, path, hash FROM docs WHERE source=?", (source,)
        ).fetchall()
        n = 0
        for doc_id, path, h in rows:
            if (path, h) not in keep:
                self.db.execute("DELETE FROM chunks WHERE doc_id=?", (doc_id,))
                self.db.execute("DELETE FROM docs WHERE id=?", (doc_id,))
                n += 1
        if n:
            self.db.commit()
            self._invalidate()
        return n

    def search(self, query_vec: np.ndarray, k: int = 5,
               source: str | None = None) -> list[dict]:
        if self._vec_cache is None:
            rows = self.db.execute(
                "SELECT c.id, c.embedding FROM chunks c"
                + (" JOIN docs d ON d.id=c.doc_id WHERE d.source=?" if source else "")
                , (source,) if source else ()
            ).fetchall()
            self._vec_cache = np.array(
                [np.frombuffer(r[1], dtype=np.float32) for r in rows], dtype=np.float32
            ) if rows else np.zeros((0, query_vec.shape[0]), dtype=np.float32)
            self._id_cache = [r[0] for r in rows]
        if len(self._vec_cache) == 0:
            return []
        scores = self._vec_cache @ query_vec
        top = np.argsort(scores)[::-1][:k]
        out = []
        for t in top:
            cid = self._id_cache[t]
            row = self.db.execute(
                "SELECT c.text, d.source, d.path, d.title, d.meta "
                "FROM chunks c JOIN docs d ON d.id=c.doc_id WHERE c.id=?",
                (cid,),
            ).fetchone()
            out.append({
                "text": row[0],
                "score": round(float(scores[t]), 4),
                "source": row[1],
                "path": row[2],
                "title": row[3],
                "meta": json.loads(row[4]) if row[4] else {},
            })
        return out

    def stats(self) -> dict:
        docs = self.db.execute("SELECT COUNT(*) FROM docs").fetchone()[0]
        chunks = self.db.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
        by_source = dict(self.db.execute(
            "SELECT source, COUNT(*) FROM docs GROUP BY source"
        ).fetchall())
        last = self.db.execute("SELECT v FROM kv WHERE k='last_reindex'").fetchone()
        return {
            "docs": docs,
            "chunks": chunks,
            "by_source": by_source,
            "last_reindex": last[0] if last else None,
        }


# --- Indexer ----------------------------------------------------------------

def read_capped(path: str, cap: int = FILE_CAP) -> str | None:
    try:
        if os.path.getsize(path) > cap:
            return None
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return None


def index_jobs(store: MemoryStore, embedder: Embedder) -> int:
    """Job specs, state/summaries and transcripts from the job store."""
    jobs_dir = os.path.join(DATA_DIR, "jobs")
    n = 0
    if not os.path.isdir(jobs_dir):
        return 0
    for job_id in os.listdir(jobs_dir):
        jdir = os.path.join(jobs_dir, job_id)
        if not os.path.isdir(jdir):
            continue
        spec = read_capped(os.path.join(jdir, "job.json"))
        if spec:
            try:
                meta = json.loads(spec)
            except json.JSONDecodeError:
                meta = {}
            n += store.upsert_doc("jobs", f"{job_id}/job.json", job_id, spec, meta, embedder) or 1
        state = read_capped(os.path.join(jdir, "state.json"))
        if state:
            try:
                meta = json.loads(state)
            except json.JSONDecodeError:
                meta = {}
            n += store.upsert_doc("jobs", f"{job_id}/state.json", job_id, state, meta, embedder) or 1
        for name in ("transcript.txt", "output.txt"):
            text = read_capped(os.path.join(jdir, name))
            if text:
                n += store.upsert_doc(
                    "job", f"{job_id}/{name}", f"{job_id} — {name[:-5]}",
                    text, {"job_id": job_id}, embedder,
                ) or 1
    return n


def index_queue(store: MemoryStore, embedder: Embedder) -> int:
    """What is pending right now + current job, so 'what's next' is answerable."""
    n = 0
    try:
        import urllib.request
        with urllib.request.urlopen(DASHBOARD_URL + "/api/pending", timeout=5) as r:
            pending = json.loads(r.read())
        if not isinstance(pending, list):
            return 0  # RabbitMQ down or no queues — nothing to snapshot
        lines = [f"pending queue snapshot {datetime.now(timezone.utc).isoformat(timespec='seconds')}"]
        for pq in pending:
            lines.append(f"queue {pq.get('name')}: {pq.get('depth')} pending")
            for job in (pq.get("jobs") or [])[:10]:
                prompt = (job.get("prompt") or "").replace("\n", " ")[:300]
                lines.append(f"  - {job.get('id')} ({job.get('type')}, prio {job.get('priority')}): {prompt}")
        store.upsert_doc("queue", "pending.json", "pending queue", "\n".join(lines), {}, embedder)
        n += 1
    except Exception as e:
        log.warning("queue index: %s", e)
    cj = os.path.expanduser("~/.config/jarvis/current-job")
    text = read_capped(cj, cap=4096)
    if text:
        store.upsert_doc("queue", "current-job", "current job",
                         f"current job: {text.strip()}", {}, embedder)
        n += 1
    return n


def index_kanban(store: MemoryStore, embedder: Embedder) -> int:
    """Epics and tasks — the scheduling board."""
    root = os.path.join(DATA_DIR, "kanban")
    n = 0
    for sub, src in (("epics", "kanban"), ("tasks", "kanban")):
        d = os.path.join(root, sub)
        if not os.path.isdir(d):
            continue
        for name in os.listdir(d):
            text = read_capped(os.path.join(d, name), cap=64 << 10)
            if not text:
                continue
            try:
                meta = json.loads(text)
            except json.JSONDecodeError:
                continue
            title = meta.get("title") or name
            status = meta.get("status") or ""
            n += store.upsert_doc(
                src, f"{sub}/{name}", title,
                text + (f"\nstatus: {status}" if status else ""),
                {"kind": sub.rstrip("s"), "status": status,
                 "model": meta.get("model", ""), "priority": meta.get("priority", 0)},
                embedder,
            ) or 1
    return n


def index_repo_docs(store: MemoryStore, embedder: Embedder) -> int:
    """READMEs and text docs in the repo (worker outputs, design notes)."""
    n = 0
    for dirpath, dirnames, filenames in os.walk(REPO_DIR):
        dirnames[:] = [d for d in dirnames if d not in (".git", "node_modules", "__pycache__")]
        for fn in filenames:
            if not fn.lower().endswith((".md", ".txt")):
                continue
            if fn.startswith(("chat-prompt", "stage")):
                continue
            path = os.path.join(dirpath, fn)
            text = read_capped(path, cap=DOC_CAP)
            if not text:
                continue
            rel = os.path.relpath(path, REPO_DIR)
            n += store.upsert_doc("repo-doc", rel, fn, text, {}, embedder) or 1
    return n


INDEXERS = [
    ("jobs", index_jobs),
    ("queue", index_queue),
    ("kanban", index_kanban),
    ("repo-doc", index_repo_docs),
]


def reindex(store: MemoryStore, embedder: Embedder) -> dict:
    t0 = time.time()
    summary = {}
    for source, fn in INDEXERS:
        # Index everything first, then drop what vanished.
        keep: set[tuple[str, str, str]] = set()
        count = 0
        try:
            count = fn(store, embedder)
        except Exception:
            log.exception("reindex: source %s failed", source)
        for path, h in store.db.execute(
            "SELECT path, hash FROM docs WHERE source=?", (source,)
        ).fetchall():
            keep.add((path, h))
        stale = store.forget_missing(source, keep)
        summary[source] = {"docs": len(keep), "new_or_changed": count, "stale_removed": stale}
    now = datetime.now(timezone.utc).isoformat(timespec="seconds")
    store.db.execute(
        "INSERT INTO kv(k,v) VALUES('last_reindex',?) "
        "ON CONFLICT(k) DO UPDATE SET v=excluded.v", (now,)
    )
    store.db.commit()
    store._invalidate()
    summary["elapsed_s"] = round(time.time() - t0, 1)
    return summary


# --- HTTP app ---------------------------------------------------------------

class IndexReq(BaseModel):
    source: str
    path: str
    title: str = ""
    text: str
    meta: dict = {}


class SearchReq(BaseModel):
    query: str
    k: int = 5
    source: str | None = None


class PurgeReq(BaseModel):
    source: str


app = FastAPI(title="Jarvis Memory Service", version="0.1.0")
_embedder: Embedder | None = None
_store: MemoryStore | None = None


def get_embedder() -> Embedder | None:
    global _embedder
    if _embedder is None and os.path.exists(os.path.join(MODEL_DIR, "model.onnx")):
        try:
            _embedder = Embedder(MODEL_DIR)
            log.info("embedder loaded from %s", MODEL_DIR)
        except Exception:
            log.exception("failed to load embedder")
    return _embedder


def get_store() -> MemoryStore:
    global _store
    if _store is None:
        _store = MemoryStore(DB_PATH)
        if store_is_empty(_store):
            if _embedder := get_embedder():
                log.info("empty index — running initial reindex")
                reindex(_store, _embedder)
    return _store


def store_is_empty(store: MemoryStore) -> bool:
    return store.db.execute("SELECT COUNT(*) FROM docs").fetchone()[0] == 0


@app.get("/health")
def health():
    return {
        "status": "ok",
        "model": get_embedder() is not None,
        "model_dir": MODEL_DIR,
        "db": DB_PATH,
    }


@app.get("/stats")
def stats():
    return get_store().stats()


@app.post("/index")
def index(req: IndexReq):
    e = get_embedder()
    if e is None:
        raise HTTPException(503, "embedding model not loaded")
    n = get_store().upsert_doc(req.source, req.path, req.title, req.text, req.meta, e)
    return {"indexed_chunks": n, "status": "ok" if n else "unchanged"}


@app.post("/search")
def search(req: SearchReq):
    e = get_embedder()
    if e is None:
        raise HTTPException(503, "embedding model not loaded")
    q = e.embed([req.query])[0]
    return {"results": get_store().search(q, k=max(1, min(req.k, 20)), source=req.source)}


@app.post("/reindex")
def do_reindex():
    e = get_embedder()
    if e is None:
        raise HTTPException(503, "embedding model not loaded")
    return reindex(get_store(), e)


@app.post("/purge")
def purge(req: PurgeReq):
    return {"removed": get_store().delete_source(req.source)}


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--reindex", action="store_true", help="re-scan all sources and exit")
    parser.add_argument("--search", metavar="QUERY", help="search and exit")
    parser.add_argument("--stats", action="store_true", help="print stats and exit")
    parser.add_argument("--port", type=int, default=int(os.environ.get("MEMORY_PORT", "8101")))
    parser.add_argument("--host", default=os.environ.get("MEMORY_HOST", "127.0.0.1"))
    args = parser.parse_args()

    if args.reindex or args.search or args.stats:
        store = MemoryStore(DB_PATH)
        embedder = get_embedder()
        if args.reindex:
            if embedder is None:
                print("no embedding model at", MODEL_DIR, file=sys.stderr)
                sys.exit(1)
            print(json.dumps(reindex(store, embedder), indent=2))
        if args.stats:
            print(json.dumps(store.stats(), indent=2))
        if args.search:
            if embedder is None:
                print("no embedding model at", MODEL_DIR, file=sys.stderr)
                sys.exit(1)
            for r in store.search(embedder.embed([args.search])[0], k=5):
                print(f"[{r['score']:.3f}] {r['source']} {r['path']}")
                print("   ", r["text"][:200].replace("\n", " "))
        return

    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
