# The agent's own CLAUDE.md

This is the local agent's personal note file. It is installed to
`~/.claude/CLAUDE.md` by `scripts/install_claude_md.sh`, so Claude Code loads
it in every session, on top of the repo's `CLAUDE.md` (which is about
conversation notes).

## Machine state — the snapshot is in every round prompt

Every round prompt starts with a **MACHINE SNAPSHOT** taken at round start
by `jarvis-stack-status` — the one command that tells you the state of
the machine: docker containers, loaded llama.cpp engines (model, quant,
context, VRAM, health), host RAM/load/uptime/disk, systemd services
(agent-run, jarvis-watchdog, plane-bridge, task-runner), agent-job queue
counts, and the swap-window state. Exit 1 means something is down (the
report still prints). If your round prompt has no snapshot (the running
jarvis-jobs binary predates the injection), run it yourself at the
start of the round.

    jarvis-stack-status            # human readable
    jarvis-stack-status --json     # machine readable

**Treat the snapshot as the current state of the machine.** Do not
re-discover the layout: no `systemctl list-units`, `ss -tlnp`,
`docker ps`, or curl to engine ports just to see what is running. Probe
deeper (`journalctl`, `nvidia-smi`, `ps`) only when the job's evidence
contradicts the snapshot, or while debugging a specific failure.

Stable facts (trust them; re-verify only when a job says otherwise):
- **Brain**: Qwen3.8-27B Q4_K_M (27.3B, 15.3 GiB), llama.cpp in the
  `jarvis-engine` container, port **11434**, 128K ctx. Holds ~18.5 GB of
  the 4090's 24.5 GB at idle → ~6 GB free; 1.5–2.7 GB free while it is
  in a conversation. This engine is serving this conversation — never
  stop or kill it (the round brief explains why and what to do when a
  job needs the 4090 free: requeue it on its spec driver).
- **iGPU helper**: Qwen2.5-3B Q4_K_M (3.4B), llama.cpp in its own
  container `jarvis-engine-igpu` (compose service `engine-igpu`), port
  **11435**, 32K ctx, Vulkan. It stays up through photo swap windows —
  only the brain container stops. Containers reach it by name
  (`http://jarvis-engine-igpu:11435`); the old host unit of the same
  name is disabled (rollback path).
- **Photo generation** needs ~12 GB of free VRAM, so it runs in the
  brain's swap window (runbook `docs/brain-swap.md`, state
  `jarvis-swap status`).
- **Services**: `agent-run` (job loop), `jarvis-watchdog`,
  `plane-bridge` (Plane webhooks), `task-runner` (kanban; dashboard on
  :8080), `jarvis-memory` (:8101), `jarvis-engine-igpu`. Plane at
  111.1.2.222:8088.

## Helper model — small, always-on, on the iGPU

When you need to change your own setup or restart something, ask the
**helper** first: a small always-on model (Qwen2.5-3B Q4_K_M) running on
the Ryzen iGPU (Vulkan, port 11435, container `jarvis-engine-igpu`) — it
costs zero CPU cores. It knows a curated setup brief (services, ports, restart
procedures, key paths — `scripts/jarvis-helper-context.md`) plus a live
stack snapshot, and it answers "not in my context" honestly for anything
else (then read the doc it points at — its pointers can be vague).

    jarvis-helper "how do I restart the voice service?"
    jarvis-helper -s NAME "follow-up"      # multi-turn session
    jarvis-helper --raw "anything"         # no setup context
    jarvis-helper --status                 # health

Expect ~13 s prompt processing + ~13 tok/s decode on the iGPU. If the
engine is down: `docker restart jarvis-engine-igpu` (or `docker compose
restart engine-igpu`). Swapping the model: replace
`/opt/jarvis-igpu/models/jarvis-igpu.gguf` (keep a `.bak`), restart the
container, re-test — the container bind-mounts `/opt/jarvis-igpu`. If the
setup changes substantially, update
`scripts/jarvis-helper-context.md` and re-run `scripts/install_helper.sh`.
Investigation + benchmarks: docs/igpu-model-handler.md.

## Brain swap — photo window (mechanical, do not re-plan)

Photo generation (art :8120 / bench :8121) needs 12 GB free of the 4090's
VRAM; the brain holds ~17.6 GB, so photos run in a swap window: the brain
stops, photo jobs run as code (the `jarvis-photo` executor, no model
round at all), the brain comes back. The
bracket is **automatic**: the queue watcher opens the window when it
pulls a `photo`-tagged job and closes it when the ready batch drains
(docs/agent-run.md, "Photo window"). Runbook: `docs/brain-swap.md`.
State: `jarvis-swap status`.

When photo work needs the GPU, in your own round:
1. `jarvis-swap handoff`, fill "In flight" + "First thing on return"
   (2-3 lines each — resume, don't re-plan).
2. Make sure the photo work is queued with the `photo` tag (image-queue
   ingestion sets it; otherwise `jarvis-jobs add ... -tags photo
   -prompt "..."` — the executor reads the prompt from the job's
   sidecar).
3. End the round — the watcher opens the window on the next pull.
4. Next brain round after the window: read `notes/brain-handoff.md`, do
   "First thing on return", verify the photos, `mkdir -p notes/archive &&
   mv notes/brain-handoff.md notes/archive/`, jarvis-notes done.

Hard rules: never run `jarvis-swap out`/`in` from a local (brain) round —
the watcher runs them between rounds; a manual run is an igpu round or a
terminal `--force`. Never stop the brain mid-round. The watchdog maintain
window re-arms the brain at the deadline no matter what; `jarvis-jobs
next` skips jobs whose engine is down, so local jobs wait through the
window (photo jobs are exempt — they run as code, gated only by the
swap window itself).

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

- `gh-pr-wait <n>` (polls every 15 s, so the waiting happens outside
  your context) must exit **0** = open and `MERGEABLE`. Exit **1** =
  `CONFLICTING` — fix it first; exit **2** = merged or closed, nothing
  left to do; exit **3** = timed out while GitHub was still computing
  (re-run it).
- If it conflicts: merge `origin/main` into the branch (in the
  branch's worktree), resolve, push, and re-run `gh-pr-wait`.

The usual conflict is `notes/conversations/agent-<date>.md` — every
round appends to it. Resolve by keeping ALL entries from both sides
(chronological order), never by dropping one side.

Then — and only then — Discord-notify with the PR link and finish the
job done. (Job chat-1793154320-3d8d, 2026-10-27: PR #83 was left
CONFLICTING and had to be reworked.)
