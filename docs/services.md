# Jarvis services — what runs on this box and how

Every service the agent has built on this machine (RTX 4090, Ubuntu 26.04),
what it does, and how to run it. Since 2026-10-27 the stack runs as a single
Docker Compose project (`docker-compose.yml` in the repo root, project name
`ai-agent-build`); the old per-service systemd units are **disabled but kept**
for rollback.

## The stack at a glance

| Service | What it does | Port (host) | Container | State |
|---|---|---|---|---|
| `engine` | The brain: llama.cpp `llama-server` serving the qwen3.8 "jarvis-brain" GGUF, 128k ctx, quantized KV cache, 2 parallel slots | 11434 (127.0.0.1) | `jarvis-engine` (nvidia runtime) | model + templates: `/opt/jarvis-engine`; slots: `/var/lib/jarvis/slots` |
| `art` | Photo generation: Z-Image-Turbo 6B (diffusers, sequential CPU offload) behind a small FastAPI app; `jarvis-art generate "..."`; Discord `!photo <prompt>` sends the result back as an attachment; photo gallery site at `http://111.1.2.27:8120` | 8120 (all interfaces — the gallery is meant to be opened from the LAN) | `jarvis-art` (nvidia runtime) | code: `/opt/jarvis-art` (pinned deploy point); model: `/var/lib/jarvis/art/model`; images: `/var/lib/jarvis/art/out` |
| `bench` | T2I model benchmark (picking the next art model): candidate models × fixed prompt set, blind swipe voting, per-model metrics; skeleton only for now (no generation yet), API at `/health` + `/api/status` | 8121 (127.0.0.1) | `jarvis-bench` (nvidia runtime, reuses the `jarvis/art:local` image) | code: `/opt/jarvis-bench` (pinned deploy point, `scripts/install_bench.sh`); db: `~/.local/share/jarvis/bench/bench.db`; images/weights: `/var/lib/jarvis/bench/{imgs,models}`; kill switch: `touch /var/lib/jarvis/bench.disabled` (idles, API 503, self re-enables on rm) |
| `chat` | Discord bot: replies to owner DMs using the local brain; bridges voice wake events to jobs | — (Discord WS) | `jarvis-chat` | `.config/jarvis` (driver, prompts, `discord.env`), `.local/share/jarvis` (agentjob store) |
| `dashboard` | Web dashboard: agent jobs, work queue, system state, PWA | 8080 | `jarvis-dashboard` | job store via `.local/share/jarvis` |
| `ha` | Publishes Jarvis headline sensors to the Home Assistant box via MQTT (auto-discovery) | — (MQTT out) | `jarvis-ha` | job store + `.config/jarvis/ha.env` |
| `voice` | Voice pipeline: wake word → STT (faster-whisper small) → brain (GLaDOS persona) → TTS (edge-tts); the `jarvis_voice` HA integration POSTs here | 8100 (LAN, plain HTTP), 8443 (LAN, HTTPS — browser mic UI) | `jarvis-voice` | code: `/opt/jarvis-voice` (pinned deploy point); whisper model: `~/.cache/huggingface`; TLS cert: `~/.config/jarvis/tls` (bind-mounted) |
| `memory` | Vector search over work logs/jobs (SQLite + all-MiniLM-L6-v2 ONNX); the agent searches it before grepping | 8101 (127.0.0.1) | `jarvis-memory` | `~/.local/share/jarvis/memory` (db + model) |
| `memory-reindex` | Re-scans all sources every 15 min (was `jarvis-memory-reindex.timer`) | — | `jarvis-memory-reindex` | same as `memory` |
| `rabbitmq` | Message broker: AMQP + MQTT (port 1883, LAN-facing for HA) + Prometheus metrics | 5672, 15672, 15692 (127.0.0.1), 1883 (LAN) | `jarvis-rabbitmq` (predates this PR) | named volume `ai-agent-build_rabbitmq_data`; config: `rabbitmq/` |
| `redis` | Live job streams: the runner XADDs the agent's stream-json output per job (`jarvis:job:<id>`, 20k entries, 24 h TTL after the job ends); the dashboard tails it for the live job view | 6379 (127.0.0.1) | `jarvis-redis` | named volume `redis_data` (AOF) |
| `worker` | Consumes the raw RabbitMQ job queue (voice/coding/background) | — (metrics 9101 when on) | `jarvis-worker` | agentjob store; started by `docker compose up -d` / `jarvis-stack` |
| `wake-queue` *(optional)* | Bridges wake-word events into the job pipeline | — | `jarvis-wake-queue` | **not running today** — start with `--profile optional` |
| `prometheus` | Scrapes everything below, 15 d retention | 9090 | `jarvis-prometheus` | named volume `prometheus_data`; config: `docker/monitoring/prometheus/` |
| `grafana` | Dashboards (provisioned from `docker/monitoring/grafana/provisioning/`) | 3000 | `jarvis-grafana` | named volume `grafana_data` |
| `node-exporter` | Host CPU/RAM/disk metrics (host rootfs bind-mounted RO) | 9100 | `jarvis-node-exporter` | — |
| `nvidia-exporter` | GPU metrics (nvidia-smi wrapper, nvidia runtime) | 3001 | `jarvis-nvidia-exporter` | — |

Not containerized, on purpose:

- **`agent-run` / `agent-resume`** — the loop that runs the agent itself
  (Claude Code rounds from the job queue). It needs the host user account,
  sudo, the repo working tree and the CLI toolchain; it is also what manages
  the stack, so it stays a host systemd unit (runs as `agent`,
  `Restart=on-failure`, inhibited by `touch /var/lib/jarvis/agent-run.disabled`).
- **`jarvis-engine-igpu`** — the always-on helper model: Vulkan llama.cpp
  on the Ryzen iGPU (Qwen2.5-3B Q4_K_M, port 11435, 127.0.0.1). The agent
  queries it via the `jarvis-helper` CLI for setup-change and self-restart
  questions; it costs no CPU cores (docs/igpu-model-handler.md). Model:
  `/opt/jarvis-igpu/models/jarvis-igpu.gguf`; deploy: `scripts/install_helper.sh`.
- **`ollama`** — installed but disabled (`Conflicts=jarvis-engine.service`);
  the engine superseded it. Left as a host option, not part of the stack.
- **CLIs** (`jarvis-jobs`, `jarvis-discord`, `enqueue`, `jarvis-reply`,
  `jarvis-migrate`, `agent-local`, `agent-run`) — not services; run on demand.
  `jarvis-jobs` store: `~/.local/share/jarvis/jobs`.

## Running the stack

```bash
cd /home/agent/ai-agent-build          # repo root (project name = ai-agent-build)
docker compose config                  # sanity check
docker compose build                   # build the jarvis/* images
docker compose up -d                   # everything that runs today
docker compose --profile optional up -d   # also wake-queue
docker compose ps
docker compose logs -f chat            # logs of one service
docker compose stop voice              # stop one (state survives)
```

Images: Go services are built from the repo at build time
(`docker/jarvis-go/Dockerfile`, `ARG BIN` selects the package); `voice` and
`memory` are Python runtimes whose code is bind-mounted from the pinned deploy
points `/opt/jarvis-voice` and `/opt/jarvis-memory` (each has a
`DEPLOYED_FROM` marker); `engine` is a minimal `ubuntu:26.04` runtime — the
host's pinned llama.cpp **b11815** binary (mounted from `/opt/jarvis-engine`,
installed by `scripts/install_engine.sh`) links glibc 2.43, which the
nvidia/cuda images (ubuntu24.04, glibc 2.39) cannot provide, so the CUDA 13.1
toolkit libs are bind-mounted from `/usr/local/cuda-13.1/lib64` instead and
the driver lib is injected by the nvidia runtime.

## Verifying the stack

```bash
docker compose ps                 # 14 services (15 with --profile optional)
curl -s 127.0.0.1:11434/v1/models | head -c 100                    # engine: serving jarvis-brain
curl -s 127.0.0.1:11435/health | head -c 60                        # igpu helper: {"status":"ok"}
curl -s 127.0.0.1:8120/health | head -c 200                        # art: model loaded + GPU
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:8120/            # art gallery: 200
curl -s 127.0.0.1:8101/health                                       # memory: status + model + db
curl -s 127.0.0.1:8100/health                                       # voice: brain + whisper ready
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:8080/            # dashboard: 200
curl -s 127.0.0.1:9090/-/healthy                                    # prometheus
curl -s 127.0.0.1:3000/api/health                                   # grafana
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:9100/metrics     # node-exporter: 200
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:3001/metrics     # nvidia-exporter (GPU): 200
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:15672/           # rabbitmq mgmt: 200
```

## Runs on boot, keeps retrying

Three layers:

1. `docker.service` is enabled; every container has `restart: unless-stopped`
   (chat: `always`), so after boot or a crash dockerd restarts them with
   backoff. A crashed service therefore comes back on its own.
2. `jarvis-stack.service` (repo: `systemd/jarvis-stack.service`, install with
   `scripts/install_stack.sh`) runs `docker compose up -d` at boot as a
   oneshot with `RemainAfterExit=yes` — belt and braces, and it re-creates
   containers that were deliberately `docker stop`ped only if you re-run it.
3. The `engine` container is the one that matters most: same flags as the old
   unit, `shm_size: 16gb`, nvidia runtime. If the brain crashes, dockerd
   restarts it (model reload ≈ 30–60 s).

## Rollback to the native units

The old units are still in `/etc/systemd/system/` (disabled), so for any
service:

```bash
docker compose stop chat
sudo systemctl enable --now jarvis-chat.service
```

…and the reverse to go back to Docker. The engine's native unit
(`jarvis-engine.service`, `Restart=always` drop-in included) is the fastest
fallback for the brain.

Never run the chat container and the host unit at the same time: both
daemons receive every owner DM on the gateway, so they double-reply and
every `!build`/`!fix` gets enqueued twice. `scripts/install_chat.sh`
enforces this on every deploy — it leaves `jarvis-chat.service`
disabled while the container runs and enables it only as a fallback when
the container is down (which is also how you repair a system where both
were left running: just re-run the script).

## Deploy points (pinned host copies, mounted into containers)

| Path | Contents | How it is updated |
|---|---|---|
| `/opt/jarvis-engine` | llama-server b11815 binary, brain GGUF, chat templates | `scripts/install_engine.sh`, `scripts/pull_models.sh` |
| `/opt/jarvis-voice` | voice service code + `auth-off.env` | deploy a branch there + `DEPLOYED_FROM` marker (see the drop-in history in `jarvis-voice.service.d/deploy.conf`) |
| `/opt/jarvis-art` | art service code (`art_service.py`) | `scripts/install_art.sh` (also downloads the model to `/var/lib/jarvis/art/model`) |
| `/opt/jarvis-memory` | memory service script | copy from the repo/branch; update `DEPLOYED_FROM` |
| `/opt/jarvis-bench` | t2i bench service code (`app.py`, `config.py`, `schema.sql`, `*.yaml`) | `scripts/install_bench.sh` (no model download; weights land in `/var/lib/jarvis/bench/models` in a later job) |

Env/secrets stay on the host under `/home/agent/.config/jarvis/`
(`agent.env`, `local.env`, `ha.env`, `discord.env`) and are bind-mounted —
nothing secret is baked into images or committed.

## Notes and gotchas

- **MQTT for HA** is the rabbitmq container's 1883 port (rabbitmq_mqtt
  plugin), user from `rabbitmq/rabbitmq.conf` — not a separate mosquitto.
- **Voice auth is OFF** (`/opt/jarvis-voice/auth-off.env`) because the
  hand-installed `jarvis_voice` HA integration (v0.2.0) doesn't send
  credentials yet; the compose file keeps `auth-off.env` after `local.env` so
  the same override applies. Re-enable by dropping the file from `env_file`.
- **node-exporter** runs with `--path.rootfs=/host` (host metrics from inside
  the container); the native unit's `--collector.systemd` is dropped because
  there is no systemd inside the container.
- **Engine image base** is `ubuntu:26.04` because the pinned llama-server
  binary links glibc ≥ 2.43 (see `docker/engine/Dockerfile`); if the binary
  is ever reinstalled against a different glibc, the base must change to
  match.
- **Grafana** uses the stock 11.4.0 config plus the provisioning dir copied
  from `/etc/grafana/provisioning` (the host `grafana.ini` was all defaults
  and `ldap.toml` is the distro sample, so neither is committed).
- Pre-2026-10-27 the same services ran as native systemd units; the units in
  `systemd/` and `scripts/jarvis-*.service` are the historical definitions and
  the rollback path.
- **Chat memory** — the `chat` container gets `MEMORY_URL=http://memory:8101`;
  before replying to a chat message it searches the memory service with the
  message and appends the top hits to the system prompt (1200-char budget,
  fails open if the service is down). Hermes additionally registers a
  `search_memory` action for explicit lookups. Code: `internal/memory`
  (Go client) and `internal/hermes/memory_action.go`.
