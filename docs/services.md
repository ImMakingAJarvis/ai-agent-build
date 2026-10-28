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
| `chat` | Discord bot: replies to owner DMs using the local brain; bridges voice wake events to jobs | — (Discord WS) | `jarvis-chat` | `.config/jarvis` (driver, prompts, `discord.env`), `.local/share/jarvis` (agentjob store) |
| `dashboard` | Web dashboard: agent jobs, work queue, system state, PWA | 8080 | `jarvis-dashboard` | job store via `.local/share/jarvis` |
| `ha` | Publishes Jarvis headline sensors to the Home Assistant box via MQTT (auto-discovery) | — (MQTT out) | `jarvis-ha` | job store + `.config/jarvis/ha.env` |
| `voice` | Voice pipeline: wake word → STT (faster-whisper small) → brain (GLaDOS persona) → TTS (edge-tts); the `jarvis_voice` HA integration POSTs here | 8100 (LAN) | `jarvis-voice` | code: `/opt/jarvis-voice` (pinned deploy point); whisper model: `~/.cache/huggingface` |
| `memory` | Vector search over work logs/jobs (SQLite + all-MiniLM-L6-v2 ONNX); the agent searches it before grepping | 8101 (127.0.0.1) | `jarvis-memory` | `~/.local/share/jarvis/memory` (db + model) |
| `memory-reindex` | Re-scans all sources every 15 min (was `jarvis-memory-reindex.timer`) | — | `jarvis-memory-reindex` | same as `memory` |
| `rabbitmq` | Message broker: AMQP + MQTT (port 1883, LAN-facing for HA) + Prometheus metrics | 5672, 15672, 15692 (127.0.0.1), 1883 (LAN) | `jarvis-rabbitmq` (predates this PR) | named volume `ai-agent-build_rabbitmq_data`; config: `rabbitmq/` |
| `worker` *(optional)* | Consumes the raw RabbitMQ job queue and runs agent jobs | — (metrics 9101 when on) | `jarvis-worker` | agentjob store; **not running today** — start with `--profile optional` |
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
docker compose --profile optional up -d   # also worker + wake-queue
docker compose ps
docker compose logs -f chat            # logs of one service
docker compose stop voice              # stop one (state survives)
```

Images: Go services are built from the repo at build time
(`docker/jarvis-go/Dockerfile`, `ARG BIN` selects the package); `voice` and
`memory` are Python runtimes whose code is bind-mounted from the pinned deploy
points `/opt/jarvis-voice` and `/opt/jarvis-memory` (each has a
`DEPLOYED_FROM` marker); `engine` is the CUDA 13.1 runtime with the host's
pinned llama.cpp **b11815** binary mounted from `/opt/jarvis-engine`
(installed by `scripts/install_engine.sh`).

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

## Deploy points (pinned host copies, mounted into containers)

| Path | Contents | How it is updated |
|---|---|---|
| `/opt/jarvis-engine` | llama-server b11815 binary, brain GGUF, chat templates | `scripts/install_engine.sh`, `scripts/pull_models.sh` |
| `/opt/jarvis-voice` | voice service code + `auth-off.env` | deploy a branch there + `DEPLOYED_FROM` marker (see the drop-in history in `jarvis-voice.service.d/deploy.conf`) |
| `/opt/jarvis-memory` | memory service script | copy from the repo/branch; update `DEPLOYED_FROM` |

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
- **Grafana** uses the stock 11.4.0 config plus the provisioning dir copied
  from `/etc/grafana/provisioning` (the host `grafana.ini` was all defaults
  and `ldap.toml` is the distro sample, so neither is committed).
- Pre-2026-10-27 the same services ran as native systemd units; the units in
  `systemd/` and `scripts/jarvis-*.service` are the historical definitions and
  the rollback path.
