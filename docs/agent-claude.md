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

## Home Assistant (the house)

HA runs at `http://111.1.2.200/` (HA 2026.10 behind a reverse proxy on port
80; 8123 is not reachable from this box).

**Use the `homeassistant` MCP server for all HA tasks.** It is registered in
user scope (`~/.claude.json`), so in every Claude session its tools are
available as `mcp__homeassistant__*` — `GetLiveContext` (current entity
states), light/climate/cover/lock control, media players, vacuum, to-do list,
and the GE/SmartHQ appliance tools. If the tools are missing or `claude mcp
list` shows it down, re-register:

    claude mcp add --transport http --scope user homeassistant http://111.1.2.200/api/mcp \
      -H "Authorization: Bearer $(sudo cat /home/nathan/HAtoken.txt)"

Credentials (never print or commit them):
- Long-lived API token: `/home/nathan/HAtoken.txt` — read with `sudo cat`.
- HA UI login (user `homeai`): `/home/nathan/homeassistantuserpw.txt` — sudo.

**REST fallback** for things the MCP tools don't cover (logbook, events, raw
service calls):

    TOK=$(sudo cat /home/nathan/HAtoken.txt)
    curl -s -H "Authorization: Bearer $TOK" http://111.1.2.200/api/states
    curl -s -H "Authorization: Bearer $TOK" -X POST \
      http://111.1.2.200/api/services/light/turn_on \
      -H 'Content-Type: application/json' -d '{"entity_id":"light.kitchen"}'

WebSocket API (logbook/events): `ws://111.1.2.200/api/websocket`, auth with
`{"type":"auth","access_token":...}` after the hello message.

Caveats: the working MCP endpoint is `/api/mcp` (streamable HTTP) — the
old `/api/mcp_server/sse` path 404s on this HA version. The Jarvis headline
sensors published via MQTT are documented in `ha/README.md`.
