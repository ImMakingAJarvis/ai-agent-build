# Incident: MOE VRAM sweep killed the agent's own brain (2026-11-04)

Job: `chat-1793850561-e3c1` (2026-11-05). Diagnosis + the failsafe that
shipped with it (`scripts/jarvis-watchdog`, systemd `jarvis-watchdog`).

Owner's report: *"during one of the benchmarking steps of the MOE work, the
agent worker never recovered."* Correct — the agent was dark from **14:25 to
19:22 PST Nov 4** (and only recovered because of a manual reboot at 19:28, not
because of any failsafe).

## 1. Timeline (2026-11-04, all PST)

| Time | What happened |
|---|---|
| ~10:04 | Docker stack recreated. The RabbitMQ volume's `rabbit@…/mqtt_retained_*.dets` file ends up **0 bytes** — from then on every RabbitMQ boot fails with `not_a_dets_file` (§3). |
| 13:46 / 14:17 | `moe-bench-100` / `moe-bench-150` finish (PRs #157/#158). The sweep's METHOD (moe-bench-100 job text): *"Free the GPU: `docker compose stop engine` … This takes the local brain model down for the job duration (expected — the sweep cannot run with it up, the card OOMs); restore in step 6."* |
| 14:17:34 | `moe-bench-200` round starts. Its first cleanup command — `pgrep -f "ftvenv/bin/ft serve" \| xargs kill` — matches the round's **own** processes (the job prompt containing that path is in `claude`'s argv) → self-kill, **exit 137**, 16 s in. Round dies without restoring anything. |
| 14:18:53 | `moe-bench-250` round starts — **on the local brain** (`transcript.txt` modelUsage: `canonicalModel: jarvis-brain`, 23 turns). The job spec says `driver: sonnet`, but the spec driver is only honored in full-auto; this was a supervised dispatch, where the operator's choice wins (a typed instruction defaults to `local`). |
| ~14:20 | The round executes step 1: `docker compose stop engine` — **stopping the llama.cpp server it is running on**. |
| 14:25:52 | Round dies on the next API call: `API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)`, `terminal_reason: api_error`, turn 23 of 23. The engine container is left stopped: its `restart: unless-stopped` policy does **not** fire on an explicit `docker stop`, and nothing else re-arms it. |
| 14:29–16:01 | **11 consecutive rounds die** — `moe-bench-250` at 23 turns, then 10 more at **turn 1** with the identical ECONNREFUSED signature (`agent.log` lines 6407519–6407711; job dirs `chat-1793139397-903d`, `pr76-app-voice`, `voice-natural-tts`, `kanban-comments`, `chat-1793835565-142a`, …). "The agent worker never recovered" is literally these 11 rows. |
| ~16:0x–19:22 | One longer round stays alive (it had been dispatched off the local brain). |
| 19:22:24 | `agent-run.service: State 'stop-sigterm' timed out. Killing.` — the stop also SIGKILLs 4 orphaned `ft` (FreeToken serve) processes from the bench rounds that were in the service's process tree. |
| 19:28:59 | `systemd-logind: Power key pressed short` → **manual reboot** (19:29:57 back up). |
| 19:31:19 | Docker auto-starts the stack; `jarvis-engine` container "Up" — but **with no network attached** (see §4), so it serves no one. |
| 23:17:31 / 23:18:57 | The 23:14 round (`agent-run: clear stale stop-signal`, PR #161) runs `sudo systemctl stop jarvis-engine` then `sudo systemctl start jarvis-engine` (sudo audit). The **systemd** `jarvis-engine.service` comes up and has served host `127.0.0.1:11434` ever since (the compose container's brain is a 3 GB-VRAM zombie; §4). |
| Nov 5 08:33 | First fully healthy local-model round again (`chat-1793849788-18bc`, requeue round, 34 turns, OK) — running on the systemd unit's brain. |

**Net:** 4 h 57 min of agent darkness (14:25 → 19:22), plus the 23:18
re-arm, plus a still-broken in-stack brain (§4).

## 2. Why the worker couldn't recover (the actual gap)

1. **A round was instructed to stop the model it runs on.** The sweep needs
   the 4090 free (brain ≈17 GB + FreeToken cache ≈ the rest → OOM), so the
   job's METHOD says `docker compose stop engine`. The intended safeguard was
   the `driver: sonnet` spec (or the iGPU driver — the `stage3-igpu` brief
   says the 3B helper exists exactly for "the MoE VRAM sweeps" and that "the
   brain may be stopped while you work — that is expected"). The safeguard
   only works if the dispatch actually honors the spec: **supervised
   dispatches do not** — the operator's button/typed answer wins, and a typed
   answer defaults to `local`. `moe-bench-250` was dispatched `local`, so it
   stopped its own brain.
2. **Nothing re-arms the engine after an explicit stop.** `restart:
   unless-stopped` deliberately ignores explicit stops. The legacy systemd
   `jarvis-engine.service` (the other brain deployment) was stopped/legacy at
   the time. A clean stop is permanent until a human or a reboot acts.
3. **The failure is silent to the loop.** Each dead round just records
   `api_error` in `agent.log` and the state file goes back to `ready`; the
   loop happily re-dispatches into the same hole (11×). No component watches
   the AI log for `Connection refused` and acts.
4. **Self-kill bug (secondary):** `pgrep -f <pattern> | xargs kill` where the
   pattern appears in the round's own command line (the full job prompt is in
   `claude`'s argv) kills the round itself. `moe-bench-200` died in 16 s this
   way — the same trap class as the 2026-10-26 broad-`pkill` incident
   (`docs/runner-spinup-die-diagnosis.md` §5).

## 3. Second, concurrent death: RabbitMQ (and the `jarvis-worker` container)

While the brain incident was unfolding, the **raw-queue worker** was already
dead for 28 h: since **10:04:53 PST Nov 4** the live RabbitMQ node's
`mqtt_retained_00006666cd76f96956469e7be39d750cc7d9.dets` was **0 bytes**
(healthy copies are 5–19 KB in older node dirs of the same volume). Every
boot fails: `BOOT FAILED … not_a_dets_file "…/mqtt_retained_…dets"`
(rabbitmq_mqtt retainer), 3,300+ restarts by 11-05. Consequences while this
round was investigating: `jarvis-worker` and `jarvis-ha` crash-looping with
`connection refused`, `task-runner` WARNing every ~15 s, HA MQTT headline
publishing dead.

**Fixed live this round:** backed up the 0-byte file to
`/var/lib/jarvis/backups/rabbitmq-dets-20261105/` (it held no data), removed
it, restarted `jarvis-rabbitmq`. Verified: broker boots ("completed with 6
plugins"), mgmt API 200, `jarvis-worker` Up and consuming (logs show the
consumers re-attaching), `jarvis-ha` Up. The retained MQTT headlines are
republished by the HA publisher; nothing was lost.

**Prevention:** the watchdog now watches `jarvis-rabbitmq` and `jarvis-worker`
and escalates a persistent crash loop to Discord with a pointer to this doc
(corrupted mnesia dets is the known case — the file is volume data, so a
human decides what to delete).

## 4. Leftover state found this round: the engine container is a zombie

`docker inspect jarvis-engine` (11-05 ~11:50 PST): the container is "Up" but
**attached to no docker network** (`NetworkSettings.Networks` empty), so the
`engine:11434` service name does not resolve from any other container. Its
llama-server is alive inside (3 GB VRAM — a partial model load) but reachable
by no one. Meanwhile host `127.0.0.1:11434` is served by the **systemd**
`jarvis-engine` unit (16.7 GB, started 23:18:57 Nov 4).

Consequences while this sat: `jarvis-worker` model-info polls fail
(`lookup engine: server misbehaving`); `jarvis-ha`/`jarvis-chat`/`jarvis-voice`
LLM calls aimed at `engine:11434` cannot resolve (chat's iGPU fallback only
triggers while a job is running, so **Discord chat LLM goes dark when the
queue is idle**); the dashboard's model view is blind.

This is a deployment conflict, not a crash, so the watchdog **escalates it
instead of auto-healing it** (auto-healing risks stealing the host port out
from under the unit that serves live rounds). **Follow-up needed — pick one
brain deployment:**

- **Option A (recommended):** the compose container is canonical (as the
  compose file and the MOE job docs say). Stop + mask the systemd unit,
  recreate the container on the compose network with its port mapping
  (`docker compose up -d engine` — must be done when no round is running on
  the brain, or the host port is double-held).
- **Option B:** the systemd unit is canonical (the de-facto state since
  23:18). Retire the compose `engine` service, point in-stack consumers at
  the unit via `host.docker.internal:11434` (unit would bind `0.0.0.0`, as
  the iGPU unit does with the same "model carries no secrets" rationale),
  free the zombie's 3 GB VRAM.

Either way: fix the boot race (containers auto-starting from docker before
`jarvis-stack.service`'s `compose up -d` runs — that's how a container ends
up "Up" with no network), and update `docs/freetoken-runner.md` / the sweep
job METHODs so "stop the brain" means **whichever deployment is canonical**.

## 5. What shipped as the fix (this PR)

| Gap (§2/§3) | Fix |
|---|---|
| Nothing re-arms the engine after an explicit stop | `scripts/jarvis-watchdog` (systemd `jarvis-watchdog`): health-probes the brain (`:11434`), iGPU helper (`:11435`), engine container, RabbitMQ, worker, and `agent-run` state; restarts dead components, **rate-limited** (3 restarts/component/hour, then escalate to Discord and cool down for an hour). |
| No watcher on the AI logs | Same service tails `~/.local/state/jarvis/agent.log` (byte-offset tracked, survives rotation) and acts on round deaths carrying `ECONNREFUSED` / `Connection refused` / `api_error`: probes the brain, restarts it if down, Discord-notifies what happened. |
| Bench jobs legally need the brain down | **Maintenance mode**: `jarvis-watchdog maintain 2h` writes a TTL'd deadline; while unexpired the brain check is skipped (and a dead bench job's TTL expiry re-arms the brain automatically). Sweep jobs must call it in step 1 and `maintain off` in step 6. |
| Local-model rounds can stop their own brain | Stage-3 local brief (in `bootstrap.sh` + applied live) now forbids it and says to requeue + note that the job belongs on its spec driver. `agent-run` also Discord-notifies when a supervised dispatch diverges from the job's spec driver (the exact `moe-bench-250` situation). |
| Worker/RabbitMQ crash-looped silently | Watchdog watches both; persistent RabbitMQ crash loops escalate with the dets-corruption pointer (fixed live this round — §3). |

Ops notes:

```
jarvis-watchdog --once        # single pass (cron/CI-friendly)
jarvis-watchdog status        # last checks, restart counts, maintenance state
jarvis-watchdog maintain 2h   # legit "brain down for a benchmark" window
systemctl status jarvis-watchdog
tail -f ~/.local/state/jarvis/watchdog.log
```

## Evidence index

- `~/.local/share/jarvis/jobs/moe-bench-200/transcript.txt` — last tool call + `Exit code 137`; span 22:17:34–22:17:50Z.
- `~/.local/share/jarvis/jobs/moe-bench-250/transcript.txt` — 23 turns, `modelUsage: jarvis-brain`, result `API Error: Connection refused … (ECONNREFUSED)`, `terminal_reason: api_error`; span 22:18:53–22:25:52Z. `state.json` back to `ready`.
- `~/.local/share/jarvis/jobs/moe-bench-{100,150,200,250}/job.json` — all four specced `driver: sonnet`, class build.
- `~/.local/state/jarvis/agent.log` lines 6407519–6407711 — the 11-turn-1 ECONNREFUSED result rows.
- sudo audit journal: `systemctl stop jarvis-engine` 23:17:31, `systemctl start jarvis-engine` 23:18:57; `agent-run.service stop-sigterm timed out` 19:22:24; `Power key pressed short` 19:28:59/19:29:14.
- `docker logs jarvis-rabbitmq` — `BOOT FAILED … not_a_dets_file …mqtt_retained_…dets`; `docker inspect` RestartCount 3,300+; the 0-byte dets in `ai-agent-build_rabbitmq_data` (backup: `/var/lib/jarvis/backups/rabbitmq-dets-20261105/`).
- `docker inspect jarvis-engine` — `NetworkSettings.Networks` empty, PortBindings set, zombie llama-server at 3 GB VRAM (nvidia-smi).
- `docs/runner-spinup-die-diagnosis.md` — the 2026-10-26 precedent (same trap class: benchmark stops production engine, broad pkill, no re-arm).
- `scripts/agent-run` (full-auto vs supervised dispatch), `internal/agentjob/run.go` `ModelFor`/`Run` (spec driver honored only via the full-auto path), `bootstrap.sh` `stage3-igpu` brief ("I picked you … because the job needs the 4090 free (for example the MoE VRAM sweeps)").
