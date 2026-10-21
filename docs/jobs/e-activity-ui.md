# E — Everything that happens goes in the database, and the UI shows it

Read only the section for your job. Background, shared by all E jobs:

- Nathan wants to see everything that happens from now on in the dashboard: build rounds, jobs, Discord traffic, model loads, GPU use, services and errors. The data comes only from the J1 database (`jarvis`, PostgreSQL), so it doesn't matter which user runs the dashboard, and Nathan can query the same data with `psql jarvis`.
- Recording starts as early as possible, so these jobs come right after J1 and D0.
- UI rule from D0: every htmx poll or swap gets an HTML fragment, never JSON. Parse templates once and render into a buffer before writing.
- Never store secrets. Message text is trimmed to 2 KB.

## E1.1 — `events` and `rounds` tables

- A migration adds two tables.
    - `events`: id, at, source (`agent-run`, `agent-job`, `worker`, `bot`, `ask`, `notify`, `ollama`, `system`), kind, level (info, warn, error), job_id, round_id, summary, detail `jsonb`.
    - `rounds`: id, driver, model, job_id, started_at, ended_at, exit_code, pr_url, log_path, note.
- Index `events(at)`, `events(job_id)` and `events(source, at)`. If J1.2 created `job_events`, fold it in: move its rows into `events` and replace the table with a view of the same name.
- Done when: the migration test passes and re-running it is a no-op.

## E1.2 — `jarvis-event` CLI and recording in `agent-run`

- New `cmd/jarvis-event`: `jarvis-event <source> <kind> <summary> [--job ID] [--round ID] [--level L] [--detail JSON]`, plus `round-start` and `round-end` subcommands that print and close a round ID. It never fails the caller: if the database is down it logs to stderr and exits 0.
- `agent-run` records:
    - round start and end (driver, model, job, exit code, PR);
    - the checkpoint question and Nathan's answer (button or typed text);
    - escalations;
    - reboots.
- Done when: CLI tests pass against `jarvis_test`, and one real round shows up in `rounds` with matching `events`.

## E1.3 — Round output to a per-round log

- `agent-run` writes each round's output to `~/.local/state/jarvis/rounds/<round_id>.log` as well as `agent.log`, and records the path in `rounds.log_path`. Keep 30 days.
- Done when: the newest round has a log file, and its path is in the database.

## E1.4 — Worker and job events

- The worker and `jarvis-jobs` write an event for every job state change (queued, claimed, started, done, failed, denied, paused or skipped), with model and duration where known.
- Done when: a test job produces the full sequence of events, in order.

## E1.5 — Discord traffic events

- `jarvis-discord notify` and `ask`, and later the bot daemon, record every message in and out: direction, kind (notify, question, answer, command, chat), job or round, trimmed text, and delivery status.
- Done when: a test with a fake session records both directions, and a live `notify` shows up in `events`.

## E1.6 — System sampler

- New `cmd/jarvis-sampler` as a systemd service. Every 30s it writes one row to a new `samples` table (migration): GPU used and total memory, utilization, power and temperature (`nvidia-smi --query-gpu`), loaded models (`ollama /api/ps`), queue depths, and `systemctl is-active` for the jarvis services, the bot and Ollama.
- On a change it also writes an event: a model loaded or unloaded, a service going up or down, VRAM above 95%.
- Keep samples for 30 days.
- Done when: the service is active, rows arrive every 30s, and stopping the bot service produces a `system` warn event within a minute.

## E2.1 — "Now" panel

- At the top of the dashboard show:
    - the current round (driver, job ID and title, elapsed time, last event);
    - the worker's current job;
    - paused flags;
    - the loaded model, VRAM and GPU power from the latest sample;
    - service status dots.
- It is an HTML partial polled every 5s.
- Done when: template tests cover idle and busy states, and the panel live matches `ps` and `nvidia-smi`.

## E2.2 — Activity timeline

- A `/activity` page lists events newest first, with filters (source, level, job, round, text search) and paging. New rows come in through a polled partial that only asks for events newer than the last one shown.
- Done when: handler tests cover filters and paging, and a `jarvis-event` written from the shell appears within 5s without a reload.

## E2.3 — Job detail page

- `/jobs/<id>` shows the job's fields, instructions (collapsed), output, PR link, dependencies, all its events, and the log of the round that ran it (the last 200 lines, with a link to the full log).
- Done when: the test renders a fixture job, and every job ID on the dashboard links here.

## E2.4 — Rounds page

- `/rounds` shows the history: driver, model, job, start, duration, exit code, PR, and a link to the log. The current round's log tail refreshes every 5s.
- Done when: tests pass, and today's rounds are listed.

## E2.5 — Discord conversation view

- `/discord` is a read-only view of every DM in and out, in chat layout, with delivery status. It reads `events`, plus `chat_turns` once D7 exists.
- Done when: tests pass, and the view shows the round checkpoints exchanged today.

## E2.6 — Charts

- Small charts from `samples` and `jobs`, rendered as inline SVG or one small JS library served locally (no CDN):
    - VRAM and GPU power over the last 24h;
    - jobs per hour by status;
    - average job duration per model.
- Done when: the charts render with fixture data and with live data.

## E2.7 — Password on the dashboard

- The dashboard listens on the LAN and now shows Discord messages, so require HTTP Basic auth when `DASHBOARD_PASSWORD` is set in `~/.config/jarvis/dashboard.env` (agent-only, 0600). Give Nathan the password through `jarvis-discord notify`, never in a commit or log. `/healthz` stays open.
- Done when: tests cover with and without the password, and opening the page from another device asks for the password.
