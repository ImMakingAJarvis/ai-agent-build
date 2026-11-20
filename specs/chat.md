---
id: chat
name: Discord bot (jarvis-chat)
summary: Nathan's Discord face — daemon commands, brain replies, job enqueue, photo commands, replies routed to the worker.
group: face
kind: container
status: active
compose: [chat]
units: [jarvis-chat.service]
binaries: [cmd/jarvis-chat, scripts/install_chat.sh]
ports: ["— (Discord gateway, outbound)"]
docs: [docs/discord.md, docs/system-frame.md]
depends_on:
  - to: discord
    why: "bot gateway and DMs"
  - to: engine
    why: "classify and chat replies (ENGINE_URL)"
  - to: engine-igpu
    why: "fallback replies while a job is running; !igpu probes its health"
  - to: memory
    why: "RAG context for replies (MEMORY_URL)"
  - to: redis
    why: "NoBrakes mode flags (JARVIS_REDIS)"
  - to: rabbitmq
    why: "capability submits and result watcher"
  - to: art
    why: "!photocheck runs the art service directly"
  - to: jarvis-jobs
    why: "!build/!fix/!igpu/!photo enqueue into the agentjob store"
  - to: agent-run
    why: "!restart/!fixnow signal files; liveness check via pgrep"
---

## Purpose

The Discord daemon. Owner DMs are either daemon commands handled in code
(`!build`, `!fix`, `!fixnow`, `!igpu` — a job pinned to the igpu driver —,
`!photo*`, `!automode`/`!auto_mode`,
`!fullauto`/`!full_auto`, `!keepgoing`, `!pause`, `!restart`,
`!restart-service`) or plain language sent to the brain with Hermes
actions (enqueue_job, set_automode, set_fullauto, submit_capability,
search_memory, check_status). A Discord reply to a bot message goes to the
running round's inbox with a 👍 and is never answered by the chatbot.

## Interfaces

- Discord gateway (one bot token, owner DMs only).
- Writes jobs into the agentjob store; writes
  `~/.config/jarvis/restart-signal` for `!restart`/`!fixnow`/`!photonow`.
- Logs every conversation to `notes/conversations/discord-<date>.md`.

## Configuration

- Compose env: `ENGINE_URL`, `FALLBACK_ENGINE_URL`, `FALLBACK_MODEL`,
  `MEMORY_URL`, `JARVIS_REDIS`, `RABBITMQ_URL`, `ART_URL`,
  `ART_GALLERY_URL` (must be the LAN IP), `DASHBOARD_ADDR`, `JARVIS_CONF`.
- `env_file: ~/.config/jarvis/agent.env`; bot token in `discord.env`.
- `pid: host` + procps in the image so the agent-run liveness check sees
  host processes.

## Data

- Reads/writes the agentjob store and photo sidecars under
  `~/.local/share/jarvis`; reads PNGs from `/var/lib/jarvis/art` (ro).
- Conversation logs under `notes/conversations/` (mounted).

## Deploy

    docker compose build chat && docker compose up -d chat

`docker restart jarvis-chat` keeps the old image. `scripts/install_chat.sh`
also builds host `jarvis-chat`/`jarvis-discord` and keeps the host unit
**disabled** while the container runs — never run both (double replies,
double enqueues, 2026-10-28 PR #108).

## Verify live

    docker logs --tail 20 jarvis-chat          # "bot(s) listening"
    docker exec jarvis-chat pgrep -af '^bash /usr/local/bin/agent-run'

Then a real DM: a daemon command gets its confirmation, a plain question
gets a reply.

## Failure modes

Most of its breakages were host assumptions that do not hold in the
container:
- `ENGINE_URL` not read → "brain stumbled" (2026-10-27, PR #92).
- No art mount → `!photo` generated but never posted (2026-10-28).
- No `JARVIS_REDIS` → mode flags unreachable (2026-11-04, PR #155).
- pgrep without procps/host PID → "agent-run is not running" lie
  (2026-11-05, PR #167).
- `check_status` shelled out to docker/journalctl → "engine status:
  unknown" (2026-11-17, PR #256). Hermes `enqueue_job` still reports
  success without a queue connection (noted in PR #256).
- Stale image turned `!fullauto on` into a job (2026-11-04).
