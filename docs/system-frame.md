# System frame — the whole Jarvis in one document

Job `chat-1793853777-c10e`, 2026-11-05. A single map of the whole system:
every service, container, database and external box, what talks to what, and
the complete Discord command surface. Verified against the live box on
2026-11-05; where the live state deviates from the intended topology it is
flagged in §8. For per-service depth, follow the index in §9.

## 1. The shape of it

One box — **111.1.2.27** (RTX 4090 + Ryzen iGPU, 128 GB RAM, Ubuntu) — runs
everything. Three external endpoints participate: Discord (cloud), the Home
Assistant box (**111.1.2.200**) and Plane (**111.1.2.222:8088**).

```
  Nathan ──Discord──► jarvis-chat ──────────────────────────────────────────┐
   ▲      DMs,        │  !build/!fix/!photo/!automode… (daemon commands)     │
   │      buttons     │  plain language → brain (Hermes actions)             │
   │                  ▼                                                      │
   │        ┌─ brain: llama.cpp "jarvis-brain" 127.0.0.1:11434 (host unit) ─┼─┐
   │        │   (qwen3.8, 128k ctx, 2 slots, ~22 GB VRAM)                    │ │
   │        └─ iGPU helper: Qwen2.5-3B 0.0.0.0:11435 (jarvis-helper,         │ │
   │            chat/voice fallback while a job is running)                  │ │
   │                                                                         ▼ │
   │   agent-run (host systemd — "the agent" = Claude Code rounds) ◄── jarvis-jobs next
   │   pulls jobs ─────────────────────────────────────────────► agentjob store
   │        │                                                     ~/.local/share/jarvis/jobs
   │        ▼                                                            ▲   ▲   ▲
   │   repo work, PRs, notes ──────────────── Discord notify ◄───────────┘   │   │
   │                                                                         │   │
   │  job entry points:                                                      │   │
   │   • jarvis-chat !build/!fix ────────────────────────────────────────────┘   │
   │   • jarvis-voice tool schedule_job (jarvis-jobs add) ───────────────────────┘
   │   • plane-bridge (Plane webhook :8102) ─────────────────────────────────────┘
   │   • NoBrakes nightly (→ agentjob store)
   │   • enqueue CLI (→ RabbitMQ raw queues)
   │
   │  face:   jarvis-voice 8100/8443 (STT→brain→TTS, PWA /app) ◄─ jarvis_voice HA integration
   │          dashboard 8080 (jobs, kanban API, live streams, Stop/Talk)
   │          jarvis-chat (Discord)
   │
   │  house:  jarvis-ha ──MQTT 1883──► HA box (auto-discovered sensors)
   │          jellyfin 8096 (Reel media server, NVENC)
   │
   └─ plumbing: rabbitmq (AMQP 5672 / MQTT 1883) ◄─ jarvis-worker (raw queues)
                redis 6379 (job streams, NoBrakes flags)
                memory 8101 (vector DB over work logs — chat, voice, the agent)
                prometheus 9090 + grafana 3000 + node/nvidia exporters
                jarvis-watchdog (host) — restarts dead brain/containers, tails agent.log
```

Layers, top to bottom:

1. **The brain** — local llama.cpp models (§2). Everything "thinking" runs
   on this box; no cloud LLM unless a job is explicitly dispatched to
   Sonnet.
2. **The agent** — `agent-run`, the host systemd loop that executes queued
   jobs as Claude Code rounds on the brain (§4.3). *This* document was
   written by it.
3. **The face** — how a human talks to it: Discord (`jarvis-chat`), voice
   (`jarvis-voice`), the dashboard, the mobile PWA.
4. **The house** — Home Assistant (sensors out, voice in, notifications)
   and Jellyfin (media).
5. **The board** — Plane (one project per epic) and the local kanban.
6. **The plumbing** — brokers, caches, the vector DB, monitoring, and the
   watchdog that keeps the rest alive.

## 2. The models (who thinks with what)

| Model | Where | Serves | Notes |
|---|---|---|---|
| **jarvis-brain** — qwen3.8 GGUF, 128k ctx, 2 parallel slots, q4_0 KV | llama.cpp on the 4090, **127.0.0.1:11434** (host unit `jarvis-engine.service`) | the agent's rounds, chat, voice, Hermes classification, `jarvis-discord ask` reasoning | ~22 GB VRAM resident; slots saved under `/var/lib/jarvis/slots`. Binary/model/templates pinned at `/opt/jarvis-engine` |
| **jarvis-igpu** — Qwen2.5-3B Q4_K_M, Vulkan on the Ryzen iGPU, 32k ctx | llama.cpp, **0.0.0.0:11435** (host unit `jarvis-engine-igpu`) | `jarvis-helper` CLI (setup questions, self-restart), **worker-busy fallback** for chat/voice LLM calls, `igpu` driver rounds | costs zero CPU cores; containers reach it via `host.docker.internal:11435`. Model: `/opt/jarvis-igpu/models/jarvis-igpu.gguf` |
| **MoE bench engine** | unit `jarvis-moe-engine` (**disabled**) | VRAM-sweep benchmark rounds | started per-sweep by the benchmark driver |
| **ollama** | host unit (**disabled**, `Conflicts=jarvis-engine`) | — | superseded by the llama.cpp engine; kept as rollback option |

**Worker-busy fallback.** While a job is `running` (the signal is the
job store: `~/.local/share/jarvis/jobs/*/state.json`), the 4090 brain is
saturated by the agent's round. `jarvis-chat` then routes classify/chat LLM
calls to `FALLBACK_ENGINE_URL` (the iGPU) and `jarvis-voice` to
`FALLBACK_OLLAMA_URL`, so Discord and voice stay responsive; both fall back
to the brain when the iGPU is down. The voice model picker can force either.

> **Current deviation (2026-11-05):** the *canonical* brain is the host
> unit on 127.0.0.1:11434. The compose `jarvis-engine` container is a
> **zombie** — `Up` but attached to no Docker network, so `engine:11434`
> (what chat/voice/ha/worker reference in compose) does not resolve in
> stack. The watchdog escalates this state to Discord and never auto-heals
> it (it would conflict with the host unit for the port). Resolution
> options are documented in `notes/agent/system.md` (2026-11-05 entry) and
> await Nathan's call. See §8.

## 3. Discord — the command reference

`jarvis-chat` (container `jarvis-chat`, `restart: always`) holds the
Discord gateway. Only the owner's DMs are ever processed. Every exchange is
appended verbatim to `notes/conversations/discord-<date>.md` (**UTC
timestamps** — the box is PST).

### 3.1 Daemon commands (handled directly, no brain involved)

| Command | What it does |
|---|---|
| `!pause` | Interrupt the running coding round and answer you first; the job is re-queued. Works on the Hermes runner bots too. |
| `!restart` | Kill the running round and re-queue its job, so the agent restarts it in a fresh conversation. Ack'd in DM. No-op between rounds or when a checkpoint question is open. |
| `!restart-service` / `!restart-service now` | Restart the `agent-run` **service** (not just the round). Defers until the current round ends unless `now`; also starts the service if it is down. Runs `agent-run-restart`. |
| `!build <title…>` | Enqueue a build job into the agentjob store (first line = title, rest = instructions). Replies with the job id. |
| `!fix <title…>` | Same, class `fix`. |
| `!photo <prompt>` | Generate a photo with the local art model (brain expands the prompt first — "thinking mind"), send it back as a DM attachment. |
| `!photoassist <prompt>` | Photo generation with iterative prompt assistance. |
| `!photocheck <prompt>` | Two passes: generate, critique, improve the prompt, regenerate. |
| `!automode on\|off` | NoBrakes nightly switch (auto-enqueue of nightly jobs). Sets `nobrakes:auto_mode` in Redis. |
| `!fullauto on\|off` | NoBrakes full-auto switch (run every ready job immediately, any time of day). Also starts the agent-run loop if it is stopped. |
| `!keepgoing on\|off` | Keep-going switch: while on, a failed auto job notifies you instead of pausing autonomous execution. |

The three NoBrakes switches also work **in plain language** ("turn on full
auto") — they are Hermes mode actions, never queued jobs.

### 3.2 Brain commands (plain language, via Hermes actions)

Anything not matching a daemon command goes to the brain, which may run an
action instead of just replying:

| Action | What it does | Access |
|---|---|---|
| `enqueue_job` | Enqueue a build job into the worker queue | full |
| `check_status` | Current job status + agent log tail | all |
| `list_jobs` | List kanban tasks and statuses | all |
| `start_engine` / `stop_engine` | Start/stop the `jarvis-engine` systemd unit | full |
| `web_search` | DuckDuckGo + Wikipedia lookup | all |
| `delegate` | Break a complex task into smaller jobs and enqueue them | full |
| `set_automode` / `set_fullauto` | The NoBrakes mode switches | full |
| `search_memory` | Explicit vector search over work logs / job history / queue | all |

### 3.3 Buttons and checkpoint questions

- **Checkpoint question** — before each job the agent-run loop asks on
  Discord (approve / skip / stop, driver choice, and the NoBrakes mode
  buttons). A ✅ reaction confirms the click. While one is open, `jarvis-chat`
  skips DMs (the `ask-open` file).
- **`jarvis-discord ask "…" [-choices a,b,c]`** — the agent (me) DMs you and
  **blocks** until you answer; used before decisions only you can make.
  `jarvis-discord notify "…"` is the non-blocking twin (phase done, PR
  links, failures).
- **Cost-gated jobs** get Approve/Reject buttons.

### 3.4 Bots and access levels

Config in `~/.config/jarvis/` (secrets: `discord.env`; multi-bot:
`BOT_N_TOKEN` / `BOT_N_ALLOWED_USERS` / `BOT_N_ACCESS`):

- **Primary bot** (`DISCORD_BOT_TOKEN` + `DISCORD_OWNER_ID`) — the basic
  chat handler with all daemon commands, full access, owner only.
- **Additional bots** — each gets the **Hermes runner** (web search,
  job enqueue, delegation) with an access level:
  - `full` — enqueue jobs, pause, chat;
  - `limited` — chat + status only (the safe default);
  - `sandboxed` — chat only.

## 4. How work flows

### 4.1 Entry points

| Source | Lands in | Path |
|---|---|---|
| Discord `!build` / `!fix`, or Hermes `enqueue_job` | **agentjob store** | `jarvis-chat` → `store.Ingest` |
| Voice ("build me X" — LLM tool `schedule_job`) | **agentjob store** | `jarvis-voice` → `jarvis-jobs add -source voice` |
| Plane board changes | **agentjob store** | `plane-bridge` (webhook :8102) → `agentjob.Open(DataDir())`; catch-up sync on every start |
| Dashboard job page (Stop work / Talk box) | job signals | writes `stop-signal` / parks the job; Talk box relays to the running round |
| `enqueue` CLI, wake bridge | **RabbitMQ raw queues** | `amqp://rabbitmq:5672` → `voice` / `fix` / `coding` / `background` |
| NoBrakes nightly (`automode` on) | **agentjob store** | auto-enqueue, at most once per UTC date (`nobrakes:nightly_flag`) |
| Kanban task with an ID in its title | — | the agent reads the board entry before building (no queue hop) |

### 4.2 The two job queues (don't mix them up)

1. **agentjob store** — `~/.local/share/jarvis/jobs/<id>/` (spec,
   `state.json`, transcript). Consumed by the **agent-run loop** via
   `jarvis-jobs next` — *the agent itself* (Claude Code rounds). This is
   where `!build`/`!fix`, voice, Plane and NoBrakes jobs land, and what the
   dashboard shows. CLI: `jarvis-jobs add/next/show/finish/stop/requeue/
   keepgoing/failnote`.
2. **RabbitMQ raw queues** — `voice`, `fix`, `coding`, `background`, plus
   location queues `jobs.gpu` / `jobs.cpu` / `jobs.igpu` (A6 placement: CPU
   and iGPU jobs run parallel to a GPU job). Consumed by the **`jarvis-worker`
   container**, which routes jobs to models via the registry, records job
   history (into the agentjob store's dir), dispatches `plan_epic` jobs to
   planner models (B2), and notifies HA. The `enqueue` CLI and the optional
   `wake-queue` container feed it.

The **worker-busy signal** both face services read is the *agentjob store*:
any job `state.json` with `status: running`.

### 4.3 The agent-run round (one job, start to finish)

`agent-run.service` (host, `Restart=on-failure`; a Discord **Stop** exits 0
and stays stopped; `touch /var/lib/jarvis/agent-run.disabled` inhibits it):

1. Pull the next job (`jarvis-jobs next`), mark it `running` in its
   `state.json`.
2. Pick the **driver** (recorded in `~/.config/jarvis/driver`; a reboot
   mid-round resumes on it via `agent-resume`):
   - `local` (default) — Claude Code on the 27B brain (:11434);
   - `igpu` — the same path on the 3B iGPU model (frees the 4090);
   - `sonnet` — the API `claude` binary, for rounds the local models are
     stuck on.
   A job that needs the 4090 free belongs on another driver — the spec's
   `driver:` field says which.
3. Run the round. A signal watcher polls `~/.config/jarvis/` once a second:
   `pause-signal` (from `!pause`), `restart-signal` (from `!restart`),
   `stop-signal` (dashboard Stop / `jarvis-jobs stop`).
4. The round ends with a pushed branch, an open PR (merge-conflict checked),
   a Discord notify with the PR link, a Plane item update
   (`jarvis-board done <ref>`), and the standing notes
   (`notes/conversations/agent-<date>.md` + `jarvis-notes` done/system/asked).
5. `jarvis-jobs finish <id> 0|1` records the result; the loop moves on.

### 4.4 NoBrakes (autonomy)

All state in Redis under `nobrakes:` (keys in
`docs/nobrakes/redis.md`; safety in `docs/nobrakes/safety.md`):

- `auto_mode` — nightly auto-enqueue is allowed at all (at most once per
  UTC date).
- `full_auto` — run every ready job immediately, any time.
- `keep_going` — a failed auto job notifies without pausing the run.
- `nightly_flag`, `running` — idempotency / single-runner marker.

Set via `!automode` / `!fullauto` / `!keepgoing` DM commands, plain
language, or `jarvis-jobs keepgoing on|off`. All default **off** and fail
safe (missing/corrupt value reads as off).

## 5. Service inventory

### 5.1 Containers (Docker Compose project `ai-agent-build`, repo root)

| Service (container) | What it does | Host port | Talks to | Talked to by |
|---|---|---|---|---|
| `engine` (`jarvis-engine`) | brain llama-server (see §2) + `/metrics` for Prometheus | 127.0.0.1:11434 (in compose; see §8 deviation) | n/a (mounts `/opt/jarvis-engine`, CUDA libs, slots) | chat, voice, ha, worker via `engine:11434` (intended); Prometheus `llama_engine` job |
| `chat` (`jarvis-chat`) | Discord bot daemon (§3) | — (Discord WSS) | brain (intended `engine:11434`), iGPU fallback :11435, memory :8101, art :8120, Redis (NoBrakes flags), agentjob store (mounted) | Nathan's DMs; writes `notes/conversations/discord-*.md` |
| `voice` (`jarvis-voice`) | voice pipeline: `/voice`, `/chat`, `/stt`, `/tts` (faster-whisper small → brain → edge-tts), tool loop (`schedule_job`, `search_jobs`), browser mic UI + mobile PWA at `/app` | 8100 (LAN, HTTP), 8443 (LAN, HTTPS) | brain `engine:11434` (intended), iGPU fallback :11435, memory :8101, agentjob store (ro, busy signal), `jarvis-jobs` CLI | HA `jarvis_voice` integration + HA satellites (wake+STT POST); LAN browsers/phones |
| `dashboard` (`jarvis-dashboard`) | web dashboard: agent jobs, live job streams (tail via Redis), kanban API (`/api/kanban/*`), Stop work / Talk box, PWA | 8080 (LAN) | agentjob store (mounted), Redis streams, brain `engine:11434` (intended), RabbitMQ (env) | humans, the agent's CLIs |
| `ha` (`jarvis-ha`) | publishes Jarvis headline sensors to the HA box every 30 s via MQTT auto-discovery (queue depths, current job, loaded model, API spend, power/energy, GPU temp/power) | — (MQTT out) | RabbitMQ MQTT :1883, agentjob store, RabbitMQ management (queue depths), `engine:11434` (intended, model), `nvidia-smi` | HA box MQTT integration (entities under device *Jarvis Agent*) |
| `worker` (`jarvis-worker`) | consumes RabbitMQ raw queues (§4.2); model routing, plan jobs (B2), job history, HA notifications, stream watcher (pauses GPU queue + stops engine while a Sunshine/Steam stream is active) | 9101 (container-only metrics) | RabbitMQ :5672, brain `engine:11434` (intended), model registry `/config/models.yaml`, Plane (plan delivery), HA | `enqueue` CLI, `wake-queue` (optional), Prometheus |
| `rabbitmq` (`jarvis-rabbitmq`) | AMQP broker + MQTT plugin + Prometheus metrics | 5672, 15672, 15692 (loopback), 1883 (LAN) | volume `rabbitmq_data` | worker, chat, dashboard, ha (all in-stack); HA box (MQTT subscribe); Prometheus |
| `redis` (`jarvis-redis`) | live job streams (`jarvis:job:<id>`, 20k entries, 24 h TTL) + NoBrakes flag store | 6379 (loopback) | volume `redis_data` (AOF, 256 MB noeviction) | chat (flags), dashboard (streams), the agent's CLIs (`$JARVIS_REDIS`) |
| `memory` (`jarvis-memory`) | vector search over work logs, job history, queue, pending jobs, repo docs (SQLite + all-MiniLM-L6-v2 ONNX): `/search`, `/stats`, `/health` | 127.0.0.1:8101 | `~/.local/share/jarvis/memory/memory.db` (mounted) | chat, voice, the agent (`jarvis-notes add` pushes here immediately) |
| `memory-reindex` | re-scans all sources every 15 min | — | same data as `memory` | — (timer loop in `command:`) |
| `art` (`jarvis-art`) | Z-Image-Turbo 6B photo generation (FastAPI) + gallery site | 8120 (LAN) | model `/var/lib/jarvis/art/model`, out dir `/var/lib/jarvis/art/out` | chat (`!photo*` via `ART_URL`), gallery browsers |
| `bench` (`jarvis-bench`) | T2I model benchmark (candidate models × prompt set, blind voting, metrics); skeleton stage | 127.0.0.1:8121 | bench db, image/weight dirs, kill switch `/var/lib/jarvis/bench.disabled` | bench UI/API |
| `jellyfin` (`jarvis-jellyfin`) | Jellyfin 12.4 media server — the Reel plan's day-1 baseline; 4090 NVENC transcoding; demo library now, NAS share (`/mnt/jarvis`) later | 8096 (LAN) | config `/var/lib/jarvis/jellyfin/config`, media ro | LAN clients (phones, TVs) |
| `prometheus` | scrapes the exporters + engine, 15 d retention | 9090 (LAN) | targets pinned to container names: `jarvis-node-exporter:9100`, `jarvis-nvidia-exporter:3001`, `jarvis-rabbitmq:15692`, `jarvis-worker:9101`, `jarvis-engine:11434` | Grafana |
| `grafana` | provisioned dashboards (hardware, worker, engine) | 3000 (LAN) | Prometheus | humans |
| `node-exporter` | host CPU/RAM/disk (host rootfs ro-mounted) | 9100 | — | Prometheus |
| `nvidia-exporter` | GPU metrics (nvidia-smi wrapper) | 3001 | nvidia runtime | Prometheus |
| `wake-queue` *(optional, profile `optional`)* | bridges wake-word events into the job pipeline | — | — | **not running today** |

### 5.2 Host systemd units (not containerized)

| Unit | What it does |
|---|---|
| `jarvis-engine.service` | **the canonical brain** (§2) — host 127.0.0.1:11434. `Restart=on-failure`, `Conflicts=ollama`. |
| `jarvis-engine-igpu.service` | the iGPU helper, 0.0.0.0:11435. |
| `agent-run.service` | the agent loop (§4.3). `Restart=on-failure` (exit 0 stays stopped), inhibited by `touch /var/lib/jarvis/agent-run.disabled`. |
| `agent-resume.service` | resumes a round after a reboot, on the same driver (`~/.config/jarvis/driver`). |
| `jarvis-stack.service` | oneshot `docker compose up -d` at boot (`RemainAfterExit=yes`) — belt and braces over container restart policies. |
| `jarvis-watchdog.service` | the failsafe (§7): probes brain :11434, iGPU :11435, engine/rabbitmq/worker containers, agent-run `failed` state, every 60 s; restarts what is dead (≤3 restarts/component/hour, then Discord escalation); tails `~/.local/state/jarvis/agent.log` for round deaths carrying the ECONNREFUSED signature; `jarvis-watchdog maintain <dur>` opens a TTL window that suppresses brain restarts (bench jobs **must** call it before stopping the engine); `maintain off` / `status` to inspect. |
| `plane-bridge.service` | receives Plane webhooks (HMAC-verified, :8102) and turns board changes into agent jobs (§4.1); catch-up sync on every start; event log `~/.local/share/jarvis/plane-bridge/events.jsonl`. |
| `ollama.service` | disabled (superseded). |
| `jarvis-{chat,voice,ha,memory,moe-engine,….service}` | the **pre-2026-10-27 native units, kept for rollback** (all disabled except the two engines above, which are the canonical deployments). Never run a container and its native unit at once (chat would double-reply and double-enqueue). |

### 5.3 CLIs (run on demand, not services)

`jarvis-jobs` (agentjob store), `jarvis-discord` (notify/ask),
`jarvis-board` (Plane: item/link/comment/state/done), `jarvis-notes`
(done/system/asked buckets + instant memory push), `jarvis-helper` (query
the iGPU setup-brief model), `enqueue` (RabbitMQ test jobs),
`jarvis-reply`, `jarvis-migrate`, `plane-epic`, `plane-bridge`,
`agent-local` / `agent-run` / `agent-reboot`, `ghapp-token` (hourly GitHub
token).

### 5.4 External boxes

| Box | Role | Connections |
|---|---|---|
| **Discord** (cloud) | the face for Nathan | `jarvis-chat` gateway (WSS); `jarvis-discord` CLIs via the same bot |
| **Home Assistant** (111.1.2.200, HA 2026.10 behind a proxy on :80) | the house | subscribes to MQTT :1883 (auto-discovered *Jarvis Agent* device, last-will availability); the hand-installed `jarvis_voice` integration (v0.2.0, auth currently off) POSTs to `jarvis-voice :8100`; HA satellites do on-device wake+STT and POST transcriptions; the worker sends job notifications (`HA_TOKEN`); the agent drives it via the `homeassistant` MCP server (`/api/mcp`) + REST fallback |
| **Plane** (111.1.2.222:8088, workspace `jarvis`) | the work board | `plane-bridge` (API + webhook receiver), the worker's plan delivery, `jarvis-board`/`plane-epic` CLIs; one project per epic (identifiers in `PLANE_PROJECTS`) |
| **NAS / Synology** | media source for Reel (future) | to be mounted at `/mnt/jarvis` and swapped into the jellyfin media mount |
| **Anthropic API** | fallback brain for stuck rounds | `sonnet` driver rounds only; also how the Reel plan series was authored |

## 6. Data stores

| Store | Where | Owner | Consumers |
|---|---|---|---|
| agentjob store | `~/.local/share/jarvis/jobs/` | agent-run / `jarvis-jobs` | agent-run, dashboard, chat, voice (busy signal), memory (index), jarvis-ha (current job) |
| kanban board | `~/.local/share/jarvis/kanban/{tasks,epics}/*.json` | the agent | dashboard API :8080 `/api/kanban/*`; read before building a kanban-referenced job (no comment system yet) |
| memory vector DB | `~/.local/share/jarvis/memory/memory.db` (+ ONNX embeddings) | `jarvis-memory` | chat, voice, the agent (`/search`), `jarvis-notes add` (instant push) |
| Redis | volume `redis_data` (AOF) | `jarvis-redis` | chat (NoBrakes flags), dashboard (job streams), CLIs |
| RabbitMQ state | volume `rabbitmq_data` | `jarvis-rabbitmq` | worker (queues), jarvis-ha (depths), Prometheus |
| engine slots | `/var/lib/jarvis/slots` | brain engine | slot resume |
| art | `/var/lib/jarvis/art/{model,out}` | `jarvis-art` | chat `!photo*` (reads PNGs to attach) |
| bench | `~/.local/share/jarvis/bench/bench.db`, `/var/lib/jarvis/bench/{imgs,models}` | `jarvis-bench` | bench |
| jellyfin | `/var/lib/jarvis/jellyfin/{config,demo}` | `jarvis-jellyfin` | jellyfin |
| plane-bridge events | `~/.local/share/jarvis/plane-bridge/events.jsonl` | plane-bridge | debugging |
| watchdog state | `~/.local/state/jarvis/watchdog/` (+ `watchdog.log`) | watchdog | — |
| conversation logs | `notes/conversations/discord-<date>.md` (jarvis-chat, UTC), `agent-<date>.md` (the agent, one `##` per session) | their writers | the agent (context), memory (indexed) |
| standing notes | `notes/agent/{done,system,asked}.md` | `jarvis-notes` | the agent, memory (indexed on add) |
| env/secrets | `~/.config/jarvis/*.env` (`discord.env`, `agent.env`, `local.env`, `ha.env`, `jellyfin.env`), `plane-webhook-secret`, `tls/` | human-managed | bind-mounted into containers — never committed, never printed |
| deploy points | `/opt/jarvis-{engine,igpu,voice,art,memory,bench}` | install scripts | the matching containers (pinned host copies with `DEPLOYED_FROM` markers) |

## 7. Boot and failsafe

1. **Container policies** — `restart: unless-stopped` everywhere
   (`chat`: `always`); dockerd brings them back after boot/crash.
2. **`jarvis-stack`** — re-runs `docker compose up -d` at boot.
3. **`jarvis-watchdog`** — the layer that noticed nothing in the 2026-11-04
   incident: probes every 60 s, restarts dead brain/iGPU/engine-ct/rabbitmq/
   worker (rate-limited, escalates to Discord), restarts agent-run only from
   the `failed` state (an intentional Stop is left alone), and tails the
   agent log for the ECONNREFUSED death signature. The brain gets a 10-min
   grace (llama.cpp needs minutes to load). Maintenance windows
   (`jarvis-watchdog maintain 2h`) legitimately hold the brain down for
   benchmark rounds.

Rollback to native units per service is still supported
(`docker compose stop <svc>` + `systemctl enable --now jarvis-<svc>`); see
`docs/services.md`.

## 8. Known deviations from the intended topology (2026-11-05)

- **Zombie engine container.** The compose `jarvis-engine` is `Up` but on
  **no** Docker network (no port mapping, `engine:11434` unresolvable in
  stack), while the host unit serves 127.0.0.1:11434. Consequences: chat/
  voice/ha/worker LLM calls to `engine:11434` fail in stack (chat's iGPU
  fallback only triggers while a job runs, so Discord chat LLM is dark when
  the queue is idle); the Prometheus `llama_engine` target is down. The
  watchdog escalates it hourly and never auto-heals (port conflict).
  Reconciliation options A/B are in `notes/agent/system.md` (2026-11-05) and
  await Nathan's call.
- **`wake-queue` not running** (optional profile) — the current voice path
  is HA-side wake+STT → `jarvis-voice`, not the in-stack wake bridge.
- **Voice auth is OFF** (`/opt/jarvis-voice/auth-off.env`) until the
  `jarvis_voice` HA integration sends credentials.
- **`ollama` disabled** (superseded by llama.cpp).
- **RabbitMQ dets corruption** (2026-11-04/05): 0-byte
  `mqtt_retained_*.dets` crash-looped the broker for ~28 h (took down
  worker + ha). Fixed by removing the corrupt files (backup in
  `/var/lib/jarvis/backups/rabbitmq-dets-20261105/`); the watchdog now
  escalates a persistent rabbitmq crash loop with a pointer to this fix.

## 9. Deep docs

| Topic | Doc |
|---|---|
| per-service runbook (ports, images, deploy points, rollback) | `docs/services.md` |
| Discord bot setup + commands (kept in sync with §3) | `docs/discord.md` |
| the agent loop, drivers, signals | `docs/agent-run.md` |
| local brain setup (11434/11435, slots) | `docs/local-brain.md` |
| iGPU helper model | `docs/igpu-model-handler.md` |
| voice channel design (satellites, wake loop, PWA) | `docs/voice-channel-design.md`, `docs/mobile-chat-app.md` |
| HA sensors | `ha/README.md` |
| NoBrakes flags / safety | `docs/nobrakes/redis.md`, `docs/nobrakes/safety.md` |
| work board (Plane) standard | `docs/work-board/NOTES.md` |
| the 2026-11-04 incident (why the watchdog exists) | `docs/worker-moe-incident-2026-11-04.md` |
| agent notes system | `docs/agent-notes.md` |
| Reel (Jellyfin) baseline | `docs/reel/jellyfin-baseline.md` |
| art / photo generation | `docs/art-model.md` |
