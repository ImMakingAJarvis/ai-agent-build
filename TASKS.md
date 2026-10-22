# Local AI Agent Build — Task List

Oct 14, 2026 · @Nathan Meyers

Hardware: RTX 4090 (24GB VRAM), 128GB system RAM, Ubuntu 24.04. Architecture: wake-word-triggered light model pinned in VRAM for voice/chat, heavy coding/background model offloaded into system RAM, RabbitMQ + Go listener for the priority task queue, Home Assistant Voice Preview Edition as the voice front end with on-device openWakeWord.

## Phase 1 — Hardware & OS

- [ ] Install Ubuntu 24.04 LTS on the new motherboard/RAM build
- [ ] Install NVIDIA drivers via `ubuntu-drivers autoinstall` (avoid `.run` installers)
- [ ] Install CUDA toolkit
- [ ] Verify 128GB RAM is fully recognized (`free -h`)
- [ ] Verify 4090 recognized and healthy (`nvidia-smi`)

## Phase 2 — Model Layer (coding tasks for Cursor)

- [ ] Write an install/setup script `scripts/install_ollama.sh` that installs Ollama and verifies the daemon is listening on `localhost:11434`
- [ ] Write `scripts/pull_models.sh` to pull the light model (e.g. `qwen3.6:27b`) and heavy model (e.g. `qwen3-coder-next`) via `ollama pull`
- [ ] Write an Ollama `Modelfile` for the light model that sets `num_gpu` so it stays fully pinned in VRAM, and one for the heavy model that allows CPU/RAM offload of inactive experts
- [ ] Write `scripts/benchmark_swap.sh` (or a small Go/Python harness) that times: light-model cold load, heavy-model cold load, and swap-back to light, logging results to a CSV
- [ ] Write `scripts/check_nvme_speed.sh` wrapping `fio` or `dd` to measure sequential read throughput and flag if below a threshold (e.g. 2 GB/s) needed for acceptable swap latency
- [ ] Write a health-check endpoint/script that confirms both models respond to a trivial prompt via the Ollama API

## Phase 2b — Conversation & always-on benchmark

Goal: decide which model is the always-ready talking agent and Home Assistant brain, and whether a tiny always-on model is needed at all. Idea: keep one model loaded in VRAM but idle, triggered by a wake word, instead of loading it on demand. These are jobs for the queue: queue them after the queue view (Phase 6, first slice) exists.

- [ ] Measure idle power draw of the whole GPU with the candidate model loaded in VRAM but not generating, versus generating. Check whether a separate tiny always-on model is worth the extra complexity
- [ ] Measure model load time from NVMe to confirm it is too slow to do after the wake word, so the model stays resident
- [ ] Measure wake-to-first-word latency stage by stage: speech to text, model time to first token (thinking off), and text to speech start. Target roughly one to three seconds
- [ ] Compare thinking off versus thinking on for conversation, since thinking mode can add many seconds before the first word
- [ ] Build a set of voice-style test tasks: Home Assistant tool calls, follow-up questions, multi-turn chat, and short answers. Have Claude Sonnet grade each model's responses
- [ ] Ask whether the best Qwen is close enough to top-tier reasoning to be the primary always-on AI managing Home Assistant, with the heavy coder swapped in only for queued jobs
- [ ] Keep the wake word source swappable: Home Assistant Voice Preview Edition now, a custom ESP32 mic node with openWakeWord or microWakeWord later, both feeding the same speech to text, model, and text to speech pipeline

## Phase 3 — Queue & Orchestration (coding tasks for Cursor)

**Build order:** after RabbitMQ and the worker, the very next thing built is the queue view (Phase 6, first slice), before any other job is queued. From the first job, the worker records start time, finish time, model and duration for every job, so history is already there once the view exists. Only after the view is up are the remaining jobs (benchmarks and the rest) added to the queue.

- [ ] Write `docker-compose.yml` for RabbitMQ with a management-plugin image, persistent volume, and a declared `tasks` exchange plus `voice`, `coding`, and `background` queues bound to it
- [ ] Define the job message schema (JSON): `id`, `type`, `priority`, `payload`, `created_at`, `source` — write it as a Go struct in `internal/queue/message.go`
- [ ] Write the Go AMQP connection/channel setup in `internal/queue/connection.go` using `amqp091-go`, with reconnect-on-drop logic
- [ ] Write the Go consumer in `internal/queue/consumer.go`: manual ack, `QoS(prefetch=1)`, one goroutine per queue (`voice`, `coding`, `background`)
- [ ] Write `pickModel(jobType, payload) (modelName string, useLocal bool)` in `internal/router/pick_model.go` — routes voice/chat jobs to the pinned light model, coding jobs to the heavy local model, and falls back to the Claude API when the local model is unavailable or job marks `forceAPI`
- [ ] Write the priority scheduler in `internal/queue/scheduler.go`: voice queue always drains first; coding queue only processes when voice queue is empty; background queue only processes when both are empty
- [ ] Write the idle-timeout unloader in `internal/models/idle_unload.go`: tracks last-used timestamp for the pinned light model, calls Ollama's unload/keep-alive-0 endpoint after N seconds idle (make N configurable via env var)
- [ ] Write an Ollama HTTP client wrapper in `internal/models/ollama_client.go`: `Generate()`, `LoadModel()`, `UnloadModel()`, hitting `localhost:11434`
- [ ] Write unit tests for `pickModel` covering: voice job, coding job, local model unavailable, forceAPI flag set
- [ ] Write a small CLI/script (`cmd/enqueue/main.go`) to manually publish a test job to each queue for local testing
- [ ] Extend `pickModel` to accept an explicit model override (e.g. task tagged "use Opus") in addition to its automatic local/Sonnet/Opus routing
- [ ] Write `internal/router/cost_estimate.go`: before dispatching any job to a paid API (Sonnet or Opus), estimate token usage/cost from the task description and model
- [ ] Write an approval gate: if estimated cost exceeds a configurable threshold (env var, default e.g. $1), block dispatch and require explicit approval (CLI prompt or notification) before the job runs
- [ ] Log every API-dispatched job's estimated vs. actual cost for later review

## Phase 3b — Inference engine & slots

Goal: one loaded model with several slots. One slot is reserved at top priority for my conversation; the other slots run background work (coding and the rest). The Go worker is the traffic cop: it sends each request to a slot, and pauses and restores jobs. No Slurm needed. The engine does the heavy lifting.

How it works: the weights stay in VRAM; what changes per job is the context (KV cache). Pausing a job means moving its KV cache to system RAM (or disk), running something else against the same loaded weights, then restoring it and continuing from the exact token. llama.cpp's server has slots with save/restore; vLLM swaps a paused request's cache to CPU RAM itself, so the worker mostly sets priorities there.

Engine staging: Ollama first, to get Qwen alive during boot. Then, with me in the loop, set up a slot-capable engine (llama.cpp server is lighter and reads the same quantized files; vLLM is heavier to set up but least work to run). Verify current feature support and versions yourself rather than assuming; slot save/restore smoothness in particular is unverified.

- [ ] Research llama.cpp server vs vLLM on the 4090 for this design and report a recommendation on Discord before committing
- [ ] Test the new engine with a small model first (it fits next to the loaded brain): prove it serves, that slots work, and that a job's cache can be saved/swapped out and restored without recomputing
- [ ] Swap rule: only one engine can hold the 27B in VRAM at a time. For the real switchover, ask me on Discord first, unload Ollama's copy, start the new engine, confirm it answers. If it fails, unload the new engine and restart Ollama. Keep Ollama installed as the rollback until the new engine has proven itself
- [ ] Announce the switch on Discord (and a home speaker if available) before going quiet, since the brain is offline during the swap
- [ ] Reserve slot 0 for my conversation at top priority; the remaining slots take coding and background jobs
- [ ] Worker: route each job to a slot, pause a background job (save its cache to RAM/disk) when my request needs room, restore and continue it afterwards
- [ ] Expose slot state (job, model, runtime, queue depth) as metrics from the worker for the dashboard and Grafana
- [ ] Optional small always-on "front desk" model (a 4B or 8B at 4-bit, or on CPU if VRAM is tight) that answers chat and voice instantly, hands heavy work to the queue, and can see how things are going (queue, jobs per slot, what is loading), fed by the same data as the dashboard. Check the VRAM budget first: the 27B takes about 17-19GB plus KV cache for the slots. Suggestion, not decided: keep its view read-only

## Phase 4 — Voice / Home Assistant Integration

- [ ] Connect Home Assistant Voice Preview Edition to the Home Assistant instance
- [ ] Confirm on-device openWakeWord detection is working on the Voice Preview Edition
- [ ] Wire wake-word trigger to create/enqueue a task in the RabbitMQ pipeline
- [ ] Route triggered audio to the light model for STT/response
- [ ] Test end-to-end: wake word → light model response → coding queue unaffected

## Phase 5 — Sandboxing & Security

- [ ] Set up Docker + NVIDIA Container Toolkit
- [x] Run Open Interpreter (or equivalent code-exec layer) inside a locked-down container — **skipped**: Claude Code IS the code-exec layer (bash, file I/O, tools). A separate sandbox is redundant.
- [ ] Keep Ollama as a host service (`localhost:11434`) so the container only needs HTTP access, not GPU passthrough
- [ ] Review container permissions / blast radius for unattended code execution

## Bootstrap script — self-hosted build kickoff

- [ ] Write `scripts/bootstrap.sh`, run once right after Ubuntu is installed, that: installs NVIDIA drivers + CUDA, installs Ollama, pulls the heavy coding model (e.g. `qwen3-coder-next`), and verifies it responds via the Ollama API
- [ ] Have `bootstrap.sh` clone the project repo (containing this task list / Phases 2–5) onto the box
- [ ] Have `bootstrap.sh` hand off to Cursor (or Open Interpreter) pointed at the local model's OpenAI-compatible endpoint, with this task list as its starting context, so it can begin working Phase 2 onward unattended
- [ ] Add a supervised checkpoint after each phase (script pauses / sends a notification) rather than letting it run all 5 phases fully unattended
- [ ] Keep Phase 1 (driver/CUDA install) itself as a manual or hosted-model-assisted step, since no local model is loaded yet at that point
- [x] Discord notify/ask helpers for the bootstrap agent (`cmd/jarvis-discord`, built on `internal/chat`, the start of the Phase 7 bot)
- [x] Resume-after-reboot for the NVIDIA driver install (`scripts/agent-reboot`, `scripts/agent-resume`, `agent-resume.service`)
- [x] Local takeover: after the driver stage, Sonnet sets up a local "brain" model on Ollama that can drive Claude Code itself (`scripts/agent-local`), proves it with `agent-local check`, and stops; after a Discord checkpoint the local model runs the rest of the build (pulling the coding model, then the queue system) in rounds approved on Discord, with Sonnet on call (`scripts/agent-run`)

**`bootstrap.sh`** — installs just enough to run Claude Code, then hands it the task list and shell access to build everything else itself (drivers, Ollama, RabbitMQ, the Go listener), delegating bulk/low-stakes coding to the local model once it's up. The script is a separate file, not included in this doc

- [ ] Save the script as `bootstrap.sh` in the repo root
- [ ] Set `ANTHROPIC_API_KEY` and `REPO_URL` before running
- [ ] Export the task list doc to `TASKS.md` in the repo so Claude has it as its brief

Note: during initial bootstrap (fresh machine → system fully up), the NVIDIA driver/CUDA stage runs on Haiku (escalating to Sonnet in the same conversation if the GPU isn't working afterwards) Sonnet then sets up the local brain model; after that the local model drives the build and Sonnet is only used for rounds you hand it on Discord — no cost-approval gate during this stage, since it's a one-time build cost. The approval gate / cost threshold logic (Phase 3) only applies once the system is live and running steady-state jobs.

## Phase 6 — Monitoring Dashboard (web)

**First slice, built right after the queue and worker, before any other jobs are queued:** a view of the queue showing the jobs waiting, how long each has been running, when each finished, and which model is handling it (and which slot, once Phase 3b exists). Past jobs must show up too, so job history is recorded by the worker from the very first job rather than at view time. Everything else below comes after.

- [ ] Build a small web dashboard (e.g. a lightweight Go + HTMX or simple React app) served locally on the box
- [ ] Show current queue contents: pending jobs per queue (voice/coding/background), their priority and status
- [ ] Show an approval queue: jobs awaiting cost sign-off, with estimated dollar cost, one-click approve/reject
- [ ] Show dollar cost for jobs dispatched to the Claude API (estimated vs. actual, per job and running total)
- [ ] Show estimated token cost / power draw for jobs run on the local model, so local compute cost is visible alongside API spend
- [ ] Show recent job history (completed/failed) with duration and which model handled each
- [ ] Show slot state once Phase 3b is in: which job and model is in each slot, runtime, and queue depth
- [ ] Expose this on the local network so it's reachable from other devices in the house

## Phase 7 — Notifications, Chat & Observability

Division of labor: the Phase 6 dashboard owns job-level views (queue, approvals, cost). Grafana owns hardware metrics. Home Assistant gets notifications plus a few headline numbers only — no task-list UI inside HA.

- [ ] Home Assistant notifications: write `internal/notify/homeassistant.go` calling the HA REST API (`notify.mobile_app_<device>`) with a long-lived access token from an env var
- [ ] Notify on: job complete/failed, approval needed (actionable notification with Approve/Reject buttons that call back to the Phase 6 dashboard API), phase checkpoint reached
- [ ] Discord bot (`internal/chat/discord.go`): two-way — message the bot to enqueue a job (`source: discord`), get status replies, and approve/reject cost-gated jobs with buttons; restrict to your Discord user ID only; token in env
- [ ] GitHub integration: an issue/PR comment with a specific label (e.g. `agent`) creates a job, and the agent posts results back as a comment/PR; allowlist your repos only
- [ ] Make the job `source` field cover voice / discord / github / cli, and route every reply back to whichever channel the job came from
- [ ] Publish headline sensors to Home Assistant (MQTT discovery or REST): queue depth per queue, current job, loaded model, today's API spend, GPU temp/power. Add one HA dashboard card linking out to the Phase 6 dashboard
- [ ] Add Prometheus + Grafana to `docker-compose.yml`, bound to the local network only
- [ ] Exporters: node_exporter (CPU/RAM/disk), an NVIDIA GPU exporter (utilization, VRAM, power draw, temp), RabbitMQ's built-in Prometheus plugin, and a `/metrics` endpoint on the Go listener (jobs by queue/model, duration, estimated vs. actual cost)
- [ ] Grafana dashboards: GPU power/VRAM/temp, CPU/RAM, queue depth over time, jobs per model, API spend per day
- [ ] Power cost: compute kWh and electricity cost from GPU/system power draw × a configurable $/kWh rate, so local job cost shows next to API cost
- [ ] Keep Discord/GitHub/HA tokens in env vars or a secrets file that is not in the repo

## Nice to have — Cursor via local model

- [ ] Investigate wiring the heavy local coding model (e.g. Qwen3-Coder-Next) into Cursor as a custom OpenAI-compatible endpoint, to offload high-volume/boilerplate coding requests off hosted API usage limits
- [ ] Keep Cursor on hosted models (Claude/GPT) for complex multi-file refactors; route only repetitive/boilerplate tasks to the local endpoint
- [ ] Benchmark local model code quality against hosted models before relying on it as a primary driver

## Queued jobs — Oct 21, 2026 (queue as work source, Discord conversation, dashboard fix, Qwen 3.8)

Added by Nathan's request. Do them in this order, one job per round (each on its own `phase-<n>-<slug>` branch and PR, as usual). J0 comes first: it makes the queue the source of all work, as intended, and moves every job below (and every other unchecked item in this file) onto the queue. After J0 is merged, take work from the queue, not from this file. D0 fixes a bug in work already shipped, so it is the first job on the queue. Q1 comes after the Discord jobs. Every job lists its acceptance criteria; a job is done only when all of them pass and the PR says how each was checked.

### J0 — Make the queue the source of all work

- [ ] **Run build work from the queue instead of TASKS.md** (branch `phase-3-queue-agent-jobs`)
    - Intent: once the queue and its view exist, all work (build work included) lives on the queue, and TASKS.md becomes history. Today it doesn't. The stage-3 brief (`STAGE3L` in `bootstrap.sh`, written to `~/.config/jarvis/stage3-local.txt`) still says "Read TASKS.md" and "Everything else in TASKS.md, in order", and `agent-run` starts every round from it. The worker cannot run build work either. In `cmd/worker/main.go` a local job is one tool-less `ollama.Generate` call whose output is discarded (`_, err := ...`). The API path logs "not yet implemented" and still marks the job completed. So the ~41 `bench-*` runs in history measured nothing.
    - Agent jobs: add a job kind for build work. The message carries `kind: "agent"`, `title`, `instructions` (the full job text including acceptance criteria), `branch`, `depends_on` (job IDs) and `driver` (`local` default, or `sonnet`). Running one means a Claude Code session with tools: `agent-local --dangerously-skip-permissions -p` for `local`, `claude --model "$AGENT_STAGE2_MODEL"` for `sonnet`. The prompt is the stage-3 rules (git, Discord, reboot) plus the job text. Store the transcript and output under `~/.local/share/jarvis/jobs/<id>/`, and record status, duration, model, PR URL (parsed from output or `gh pr list --head <branch>`) and a short summary in history.
    - Who runs them: the `agent-run` stage-3 loop. Each round, it takes the next ready agent job (highest priority, then oldest, with all `depends_on` done) and names it in the Discord checkpoint ("Next: D0 — Fix the dashboard poll… Local model / Sonnet / Skip / Stop"). Then it runs that job on the chosen driver and records the result. Typed instructions still apply to that round. Keep the Discord checkpoint between rounds, since that is how Nathan approves work. Run one agent job at a time: the GPU serves one sequence (`-np 1`).
    - Do not hold a RabbitMQ message unacked for a whole round: the broker's consumer ack timeout (30 min by default) closes the channel and redelivers. Claim instead. Take the message, write a `claimed` record (job ID, round, time) to the job store, then ack. A claimed job left without a result (crash or reboot) goes back to ready on the next start and is not run twice at once. Text jobs (`voice`, chat) keep their current fast path.
    - Fix the worker so nothing lies: store the generated text for text jobs, and mark API-dispatched jobs `failed` with "API dispatch not implemented" until it is. Drop the paused-queue skip path that used to ack and lose jobs if any copy of it remains.
    - Migration: `scripts/migrate_tasks_to_queue` enqueues, in this order, J0's successors from this section (D0, D1-D8, Q1), then the benchmarks (as agent jobs, because the earlier runs measured nothing), then every other unchecked item in this file in file order. Each gets a stable ID (slug of section and title) and `depends_on` where the text gives an order. It is idempotent: running it twice adds nothing. Then put a note at the top of TASKS.md: "Work now lives in the queue (see the dashboard); this file is history." Tick J0.
    - Briefs: change `STAGE3L` and `STAGE3S` in `bootstrap.sh` and the live `~/.config/jarvis/stage3-local.txt` and `stage3-sonnet.txt` so a round does the queued job it was handed, instead of picking from TASKS.md. Replace `/usr/local/bin/agent-run` and the brief files atomically (write a temp file, then `mv`), because bash reads a running script as it goes and a round may be in progress. Do not restart or kill a running round.
    - Dashboard: show agent jobs with title, status (ready, claimed, running, done, failed), driver, PR link and dependencies.
    - Acceptance criteria:
        - `go build ./...` and `go test ./...` pass. Tests cover these cases. An agent job with a fake agent command is claimed, run, and recorded with output and PR URL. A failing agent command is recorded `failed` with its error. A job whose `depends_on` is not done is skipped for the next ready one. A claimed job with no result is ready again after a restart and is not run twice. A fake job that runs longer than the ack timeout (shorten the timeout in the test broker) does not get redelivered. A text job's output is stored. An API-routed job is `failed`, not `completed`.
        - The migration test runs against a copy of this file: the order is J0's successors, then the benchmarks, then the rest. A second run adds nothing.
        - Manual: after merge and migration, the dashboard lists the queued agent jobs with D0 first. The next Discord checkpoint names D0. That round opens the D0 PR, and the job shows `done` with the PR link. The round after that names D1 without anyone mentioning TASKS.md.

### D0 — Dashboard bug: raw JSON replaces the UI after a few seconds

- [ ] **Fix the dashboard poll that swaps JSON into the page** (branch `phase-6-dashboard-poll-fix`)
    - Root cause: in `internal/dashboard/templates/index.html` (main as of b5e96d7, line 300) the `<div id="dashboard">` has `hx-get="/api/state" hx-trigger="every 5s" hx-swap="outerHTML"`. `/api/state` is `handleState` in `internal/dashboard/dashboard.go` (line 198, route on line 163), which returns `application/json` through `respondJSON` (line 560). htmx swaps any 2xx body in as-is, so on the first poll (about 5s plus the time `gatherState` takes: Ollama health check, RabbitMQ depths, a full read of `jobs.jsonl`) the whole dashboard div is replaced by the JSON text. That text carries no `hx-*` attributes, so polling stops and the raw JSON stays on screen. That also wipes out the nested approvals panel (`<div id="approvals">`, line 393). Also, `<body>` on line 298 polls the same endpoint every 5s with `hx-swap="none"`, which doubles the load and does nothing. Keep `/api/approvals` (which already returns HTML) working.
    - Fix: keep `/api/state` as JSON for scripts and the Discord bot. Wrap the dashboard markup in `{{define "dashboard"}}...{{end}}` inside `index.html`, render it in place from `handleIndex`, and add `GET /partials/dashboard` (`text/html; charset=utf-8`) that executes only that block from the same `gatherState()` data. Point the div's `hx-get` at `/partials/dashboard`; the returned fragment must be the full `<div id="dashboard" hx-get="/partials/dashboard" hx-trigger="every 5s" hx-swap="outerHTML">…</div>` so polling continues after each swap. Remove the `hx-*` attributes from `<body>`. Parse the template once at startup instead of on every request.
    - Acceptance criteria:
        - `go build ./...` and `go test ./...` pass. New `httptest` tests: `/partials/dashboard` returns 200, `Content-Type: text/html`, a body that starts with `<div id="dashboard"` and contains `hx-get="/partials/dashboard"`, and no `{"ollama_healthy"`; `/api/state` still returns valid JSON with `application/json`; `GET /` contains exactly one `hx-trigger="every 5s"` for the dashboard (no body poll).
        - Manual: open `http://<box>:8080/` and leave it for 60s; no JSON is ever shown. Enqueue a job with `jarvis-enqueue` and it appears within about 10s without a reload.
        - After Nathan merges, rebuild and restart the dashboard (only the dashboard process) and repeat the manual check. Put the before/after in the PR.
    - Also in this job: keep the dashboard and worker up. On Oct 21 the dashboard and workers were started by hand from agent sessions (`/tmp/jarvis-dashboard`, `/tmp/jarvis-worker`, two copies of `~/.local/bin/worker`), so the dashboard went down when a round ended (`dashboard: shutting down` at 15:37) and three workers were consuming at once. The 15:32 dashboard build also failed mid-render: `/tmp/dashboard.log` shows `superfluous response.WriteHeader call from ...(*Server).handleIndex (dashboard.go:194)`, which means `tmpl.Execute` failed after it had started writing, so the page was cut off. Find and fix that template error, and render into a buffer before writing so a failure returns a clean 500 instead of half a page.
        - Install `jarvis-dashboard.service` and `jarvis-worker.service` (systemd, `User=agent`, binaries in `/usr/local/bin` or `~/.local/bin`, not `/tmp`, `Restart=on-failure`, enabled at boot, logs to the journal). Stop the stray hand-started copies so exactly one worker runs. Never start these from an agent session again; use `systemctl restart`.
        - Acceptance criteria: a test renders `index.html` with fixture state (including empty history, failed jobs and approvals) without error. `systemctl is-active jarvis-dashboard jarvis-worker` is `active` for both, and `pgrep -c -f 'jarvis-worker|bin/worker'` is 1. The dashboard is still up after the round that did this ends, and after a reboot.

### D1-D8 — Two-way Discord conversation (refines the Phase 7 Discord bot item)

Context for all D jobs: today `cmd/jarvis-discord` (`internal/chat/discord.go`, discordgo) is already a bot, not a webhook. `notify` is REST only. `ask` opens a short-lived gateway session with only `IntentsDirectMessages`, sends a DM to `DISCORD_OWNER_ID`, and takes the first reply from the owner in that DM. Config is `DISCORD_BOT_TOKEN` and `DISCORD_OWNER_ID` in `~/.config/jarvis/discord.env` (never log, print or commit them). `agent-run` relies on `jarvis-discord ask` for every round checkpoint, so it must keep working at every step. The conversation stays in Nathan's DM with the bot. Discord sends message content in DMs without the privileged Message Content intent, so no portal change is needed for DMs. An optional guild channel (D1) needs that intent; Nathan has to turn it on by hand.

- [ ] **D1 — Long-running bot daemon with owner-only intake** (branch `phase-7-discord-daemon`)
    - Add `cmd/jarvis-bot` (or a `jarvis-discord serve` subcommand) that keeps one gateway connection open, reconnects on drop, and receives every `MessageCreate` in the owner DM. Accept a message only if the author ID equals `DISCORD_OWNER_ID`, the author is not a bot, and the channel is the owner DM (or `DISCORD_CHANNEL_ID` if set, see below). Drop everything else silently, with a debug log line that has no content. Add a simple rate limit (for example 20 messages a minute) and a 4000-character input cap.
    - Optional channel: if `DISCORD_CHANNEL_ID` is set in `discord.env`, also accept owner messages in that one guild channel. This needs `IntentsGuildMessages | IntentsMessageContent` and is only used when the variable is set.
    - Install it as `jarvis-bot.service` (systemd, `User=agent`, `EnvironmentFile=~/.config/jarvis/discord.env`, `Restart=on-failure`). For now it only replies "received" with a ✅ reaction; real handling comes in D3-D6.
    - Update `docs/discord.md` with the manual steps: nothing for DMs; for the optional channel, enable **Message Content Intent** under Bot → Privileged Gateway Intents and give the bot View Channel, Send Messages, Read Message History and Add Reactions in that channel; how to copy the channel ID.
    - Acceptance criteria: unit tests for the filter (owner DM accepted; other user, bot author, other channel, and over-length message rejected) using fake `MessageCreate` events. `systemctl status jarvis-bot` is active and survives `systemctl restart`. A DM from Nathan gets ✅. A DM from a second account gets nothing. No token appears in logs (`grep` the journal for the first 10 characters of the token: no hits).

- [ ] **D2 — Make `jarvis-discord ask` and the daemon share the DM safely** (branch `phase-7-discord-ask-coexist`)
    - Problem: once the daemon listens, a reply meant for an open `ask` question would also be treated as chat or a command. Fix: the daemon becomes the single owner of incoming DMs. It serves a local Unix socket (`$XDG_RUNTIME_DIR/jarvis-bot.sock` or `~/.local/state/jarvis/bot.sock`, mode 0600). `jarvis-discord ask` registers its question through the socket and blocks for the answer; the daemon routes Discord replies and button clicks to open questions exactly as `internal/chat` does today (Discord reply to the question message, else the oldest open question). Messages that start with the command prefix (D3) are never treated as answers. If the socket is missing, `ask` falls back to today's behaviour so `agent-run` never breaks.
    - Do not edit `/usr/local/bin/jarvis-discord` in place while `agent-run` may be calling it: install the new binary atomically (`install` to a temp name, then `mv`).
    - Acceptance criteria: tests for routing (pending question gets the plain reply; `!status` with a question pending goes to the command handler; button click goes to the right question; two open questions plus a Discord reply go to the referenced one). Manual: with the daemon running, `jarvis-discord ask -choices "yes,no" "test?"` returns the clicked button and a typed reply; with the daemon stopped it still works. `agent-run`'s round checkpoint still works end to end.

- [ ] **D3 — Command parser and control commands** (branch `phase-7-discord-commands`)
    - Commands start with `!` (also accept `/` text). Parse into a typed command with arguments, case-insensitive, with a `!help` listing. Unknown commands reply with help, not an error dump.
    - Control commands: `!pause [jobs|build|all]` and `!resume [jobs|build|all]`. `jobs` pauses the worker's consumption from RabbitMQ (reuse the scheduler's pause, exposed through a small local control endpoint or a state file the worker watches; in-flight jobs finish, nothing is dropped). `build` writes `~/.config/jarvis/paused`; teach `agent-run` to check that file before starting a new round and to wait (with a Discord notice) until it is removed. It must never kill a running round. Replace `agent-run` atomically, because bash reads scripts while running.
    - Commands that drop or change work (`!cancel`, `!move`, D5) ask for confirmation with Yes/No buttons first.
    - Acceptance criteria: table-driven parser tests (every command, aliases, missing or extra args, quoting, unknown command). `!pause jobs` stops new jobs within 5s (enqueue a test job: it waits; `!resume jobs`: it runs). `!pause build` while a round runs: the round finishes and the next one does not start until `!resume build`. State survives a daemon restart.

- [ ] **D4 — Status and progress queries** (branch `phase-7-discord-status`)
    - Move the dashboard's `gatherState()` and job-history reading into a shared package (for example `internal/status`) used by both the dashboard and the bot, so the numbers always match.
    - `!status`: build stage (`~/.config/jarvis/stage`), who drives the current round (`driver`) and whether a round is running, paused flags, the current job (started in `jobs.jsonl` with no finish), queue depths for voice, coding and background, Ollama health and the loaded model (`/api/ps`), and VRAM used. `!queue`: pending jobs per queue (id, type, age). `!jobs [n]`: the last n finished jobs with status, model and duration. `!failures [n]`: recent `failed` and `denied` jobs with a one-line error. `!prs`: open PRs (via `ghapp-token` and `gh`). Replies use short Discord embeds or code blocks and are split under 2000 characters.
    - Acceptance criteria: unit tests feed fixture `jobs.jsonl` and fake queue depths and check the formatted output (including a job that started but did not finish, and the empty case). Manual: each command answers within 3s and matches the dashboard at the same moment.

- [ ] **D5 — Job commands: add, cancel, reprioritize** (branch `phase-7-discord-jobs`)
    - `!add [voice|coding|background] <prompt>` publishes a job through the existing `internal/queue` code with `source: "discord"` and a `reply_to` holding the DM channel and message ID, and answers with the job ID. `!cancel <id>` removes a pending job. `!move <id> <queue>` (reprioritize) moves a pending job to another queue by taking it off its queue and republishing it with the new type and priority, keeping its ID and `created_at`; other messages are requeued untouched. Running jobs cannot be cancelled or moved, so say so. Cost-gated jobs still go through the approval gate.
    - When a job with `source: discord` finishes or fails, DM the result (trimmed, with its ID) back to `reply_to`. This is the first piece of "route every reply back to the channel it came from".
    - Acceptance criteria: tests against a test RabbitMQ vhost (or the docker-compose broker with throwaway queues): add creates a message with the right fields; move keeps the ID and lands in the target queue; cancel on a running or unknown ID returns a clear message and changes nothing; other jobs in the queue are untouched and in order. Manual: `!add background say hi` gets an ID, appears on the dashboard, and the result arrives as a DM.

- [ ] **D6 — Free-form chat with the AI** (branch `phase-7-discord-chat`)
    - Any owner message that is not a command and not an answer to an open `ask` is chat. Send it as a `voice` job (`source: discord`, top priority, with `reply_to`) so it goes ahead of coding and background work. Use the local brain (`LOCAL_MODEL`), never a paid API unless Nathan writes `!ask-claude ...`, which still goes through the cost gate. The system prompt says who it is (the Jarvis build system), includes a short fresh `!status` snapshot so "how's it going?" gets a real answer, and says it cannot run commands from chat but can point Nathan to the `!` commands.
    - The GPU runs one sequence at a time (`-np 1`) and the build agent may be mid-request, so show Discord's typing indicator while waiting, send "busy, queued behind the build agent" after 20s, and time out after 5 minutes with a clear message. Strip any thinking output before replying. Split long replies.
    - Acceptance criteria: tests with a fake model client: chat is routed to a `voice` job and the reply goes to the right channel; command-prefixed text and pending-ask answers are not routed to chat; timeout path. Manual: "hi, what are you working on?" gets a sensible reply that mentions the current stage or job; a long answer arrives in several messages.

- [ ] **D7 — Persistent conversation context** (branch `phase-7-discord-memory`)
    - Store each turn (role, text, time, related job ID) in `~/.local/share/jarvis/discord/conversation.db` (SQLite) or a JSONL file, mode 0600, outside the repo. Each chat request sends the last N turns that fit a token budget (default 6000 tokens, configurable) plus a rolling summary of older turns, refreshed by a background job when the history grows past the budget. `!reset` starts a new conversation (the old one is kept on disk), `!forget` deletes it after a confirmation button, and `!context` shows how many turns and tokens are in use.
    - Acceptance criteria: tests for the token-budget windowing, summary insertion, `!reset` and `!forget`, and reload after restart. Manual: tell it "my favourite colour is green", restart `jarvis-bot.service`, ask "what's my favourite colour?" and it answers green; after `!reset` it no longer knows.

- [ ] **D8 — End-to-end check, docs and hand-off** (branch `phase-7-discord-e2e`)
    - Write `docs/discord-bot.md` (commands, how routing works, where state and history live, how to restart, how to turn the optional channel on) and link it from `docs/discord.md`. Add a scripted smoke test (`scripts/discord_smoke.sh`) that checks the daemon, socket and worker are up and runs the parser and status tests.
    - Acceptance criteria: a checklist in the PR, done live over Discord: `!help`, `!status`, `!queue`, `!jobs`, `!failures`, `!add` with the result DM'd back, `!move`, `!cancel`, `!pause` and `!resume` for jobs and build, chat with memory across a restart, an `agent-run` round checkpoint answered while the daemon runs, a message from another account ignored. Then notify Nathan with the PR link.

### Q1 — Upgrade the local brain from Qwen 3.6 to Qwen 3.8

- [ ] **Switch the brain to `qwen3.8:27b`, keep 3.6 for rollback** (branch `phase-2-qwen38-brain`)
    - Current setup: Ollama 0.40.7 (`ollama.service`), brain `jarvis-brain` built from `models/brain.Modelfile` (`FROM qwen3.6:27b`, Q4_K_M, `num_ctx 65536`, `num_gpu 999`, `temperature 0.6`, `top_p 0.95`, `top_k 20`), with MTP drafting (`draft_num_predict 3`) and the vision projector. `~/.config/jarvis/local.env` has `LOCAL_MODEL=jarvis-brain`; `agent-local` sets `CLAUDE_CODE_MAX_CONTEXT_TOKENS=65536`. The worker and the idle unloader refer to `jarvis-brain` by name. VRAM on Oct 21: about 22.3 GB used by Ollama's runner and 23.6 of 24.5 GB on the card.
    - Target: `qwen3.8:27b` (same as `latest` and `27b-mtp-q4_K_M`: 27B Q4_K_M, 16.8 GB weights plus a 0.93 GB projector, MTP draft enabled, needs Ollama ≥ 0.32.12). Its weights are the same size as 3.6's, so it should fit at 64K context with the same headroom. Fallback if MTP drafting misbehaves: `qwen3.8:27b-q4_K_M` (same weights, no draft params). Q8_0 (30 GB) and BF16 do not fit in 24 GB; the MLX and nvfp4 tags are not for this runtime.
    - The brain cannot safely test its own replacement while it is loaded (two copies do not fit in VRAM). If you are the local model, do the first two steps, open the PR, and ask Nathan on Discord for a Sonnet round to do the rest. Do not run any of it while a Discord chat or queued job is in flight: `!pause jobs` first (D3) and `!resume jobs` afterwards.
    - Steps:
        1. `ollama pull qwen3.8:27b`. Keep `qwen3.6:27b`. `ollama cp jarvis-brain jarvis-brain-qwen36` (the rollback copy).
        2. Add `models/brain-qwen38.Modelfile`: `FROM qwen3.8:27b`, `PARAMETER num_ctx 65536`, `PARAMETER num_gpu 999`, keeping the registry's sampling defaults unless the smoke test shows tool-call problems (then try `temperature 0.6`). `ollama create jarvis-brain-qwen38 -f models/brain-qwen38.Modelfile`.
        3. Smoke test, with nothing else using the GPU: `LOCAL_MODEL=jarvis-brain-qwen38 agent-local check` passes; `ollama ps` shows 100% GPU and context 65536; `nvidia-smi` shows at least 0.8 GB free; a `/v1/messages` request with a tool definition returns a well-formed `tool_use`; a voice job through the worker completes; decode speed (`ollama run --verbose`) is not more than 15% slower than 3.6. Record the numbers.
        4. Ask Nathan on Discord before switching. Switch with `ollama cp jarvis-brain-qwen38 jarvis-brain` (so the worker, idle unloader and `local.env` stay unchanged), then `ollama stop jarvis-brain` so the next request loads the new one, and run `agent-local check` again. Change `models/brain.Modelfile` to `FROM qwen3.8:27b`.
        5. Rollback, documented in `docs/local-brain.md` and tested once: `ollama cp jarvis-brain-qwen36 jarvis-brain && ollama stop jarvis-brain && agent-local check`. Do not `ollama rm qwen3.6:27b` until Nathan says so.
    - Acceptance criteria: both smoke tests (step 3 and after step 4) pass with the numbers in the PR; the rollback command was run once and passed, then switched forward again; `docs/local-brain.md` is updated (model, quant, VRAM, context, why, rollback); a Discord notify with the PR link and VRAM before and after.
