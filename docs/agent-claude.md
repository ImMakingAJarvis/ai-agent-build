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

It can also be driven as a CLI
(`python3 scripts/memory_service.py --search "..." | --stats | --reindex`).
If the service is down, run `sudo systemctl restart jarvis-memory`.

## Keep notes — things done, the system, what Nathan asked

Besides the per-run conversation notes (`notes/conversations/agent-<date>.md`),
keep a small standing record in three buckets under `notes/agent/`, using
`jarvis-notes`:

- `done`   — a curated "shipped" log (one line per finished job/PR)
- `system` — durable facts about this machine/stack (services, ports, gotchas)
- `asked`  — themes of what Nathan has asked about / decided

At the end of each job, record what is worth keeping:

    jarvis-notes add done "<what shipped> (PR #n)" --job <id>
    jarvis-notes add system "<new durable fact, if any>"
    jarvis-notes add asked "<something Nathan asked or decided, if any>"

Each `add` writes the file AND pushes it into the vector graph immediately, so
a note is searchable this session rather than after the 15-minute reindex.
Search the graph (above) before grepping the filesystem for prior work.
See `docs/agent-notes.md` for the full how-it-works.

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

## Before you call a PR done

A PR is not done while it conflicts with main. Before finishing any
job that opens or updates a PR, verify the branch has no merge
conflict:

- `GH_TOKEN=$(ghapp-token) gh pr view <n> --json mergeable` must say
  `MERGEABLE`. `UNKNOWN` means GitHub is still computing — wait and
  re-check; `CONFLICTING` means fix it first.
- If it conflicts: merge `origin/main` into the branch (in the
  branch's worktree), resolve, push, re-check.

The usual conflict is `notes/conversations/agent-<date>.md` — every
round appends to it. Resolve by keeping ALL entries from both sides
(chronological order), never by dropping one side.

Then — and only then — Discord-notify with the PR link and finish the
job done. (Job chat-1793154320-3d8d, 2026-10-27: PR #83 was left
CONFLICTING and had to be reworked.)
