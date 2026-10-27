# The agent's own CLAUDE.md

This is the local agent's personal note file. It is installed to
`~/.claude/CLAUDE.md` by `scripts/install_claude_md.sh`, so Claude Code loads
it in every session, on top of the repo's `CLAUDE.md` (which is about
conversation notes).

## The vector graph — more data than the repo

The **Jarvis Memory Service** (`jarvis-memory` systemd unit,
http://localhost:8101) is a local vector database that indexes the work logs
this agent produces. It holds data that is NOT in the repo working tree:
every past job's spec, state file and transcript, the pending queue, and a
snapshot of the repo docs. Fully local (SQLite + all-MiniLM-L6-v2 ONNX
embeddings).

**Search it before grepping the filesystem** for "what did we do about X",
"what happened in job X", or "what is queued next":

    curl -s -X POST localhost:8101/search -H 'Content-Type: application/json' \
      -d '{"query":"wake word latency","k":5}'

Endpoints:
- `POST /search` — `{"query": "...", "k": 5, "source": "job|jobs|repo-doc|queue"}`
  → top-k chunks, each with `source`, `path`, `title`, `meta`, `score`.
- `GET /stats` — doc/chunk counts per source and last reindex time.
- `GET /health` — service and embedding-model status.

It reindexes automatically every 15 minutes (`jarvis-memory-reindex.timer`).
Data lives in `~/.local/share/jarvis/memory/memory.db`.

Caveat: the service code (`scripts/memory_service.py`) currently lives on
branch `phase-chatbot-memory-vectordb`, not main. It can also be driven as a
CLI (`python3 scripts/memory_service.py --search "..." | --stats | --reindex`).
If the service is down, restore the script from that branch and run
`sudo systemctl restart jarvis-memory`.
