# Local AI Agent Build — Task List

> **Work now lives in the queue (see the dashboard); this file is history.** Do not read it whole: it is large. Your job text comes from the queue.

Oct 14, 2026 · @Nathan Meyers

Hardware: RTX 4090 (24GB VRAM), 128GB system RAM, Ubuntu 24.04. Architecture: wake-word-triggered light model pinned in VRAM for voice/chat, heavy coding/background model offloaded into system RAM, RabbitMQ + Go listener for the priority task queue, Home Assistant Voice Preview Edition as the voice front end with on-device openWakeWord.

## Job rules — read these first, every round

- Keep every job tiny. The local brain has a 64K-token context and Claude Code itself uses a big part of it. A job must be finishable in one round while reading at most about three source files plus its own section of its job doc. On Oct 21 a round died from "Autocompact is thrashing" because the work did not fit.
- If a job looks bigger than that, split it before writing any code. Add the smaller jobs right after it in the index below, with matching sections and acceptance criteria in its job doc. Commit the split, then do only the first one.
- Read only what you need: your index line and your section of the job doc. Never read big files whole. `grep -n` first, then read line ranges. For example, don't read `bootstrap.sh` (24 KB); grep for the heredoc you need. Cut long command output with `| tail -n 50` or `| head -c 4000`.
- One job is one small PR. Tick the job in the index when its PR is open.

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

These items are now queue jobs, group S in the index below (`docs/jobs/s-slots.md`). Note: the loaded brain uses about 23.4 GB of 24.5 GB, so nothing else fits next to it on the GPU.

- Research llama.cpp server vs vLLM on the 4090 for this design and report a recommendation on Discord before committing *(queued as S2.1-S2.3)*
- Test the new engine with a small model first (it fits next to the loaded brain): prove it serves, that slots work, and that a job's cache can be saved/swapped out and restored without recomputing *(queued as S3.1-S3.4)*
- Swap rule: only one engine can hold the 27B in VRAM at a time. For the real switchover, ask me on Discord first, unload Ollama's copy, start the new engine, confirm it answers. If it fails, unload the new engine and restart Ollama. Keep Ollama installed as the rollback until the new engine has proven itself *(queued as S4.1-S4.4)*
- Announce the switch on Discord (and a home speaker if available) before going quiet, since the brain is offline during the swap *(queued as S4.4)*
- Reserve slot 0 for my conversation at top priority; the remaining slots take coding and background jobs *(queued as S5.1)*
- Worker: route each job to a slot, pause a background job (save its cache to RAM/disk) when my request needs room, restore and continue it afterwards *(queued as S1.1-S1.4, S5.2, S5.3)*
- Expose slot state (job, model, runtime, queue depth) as metrics from the worker for the dashboard and Grafana *(queued as S5.4)*
- Optional small always-on "front desk" model (a 4B or 8B at 4-bit, or on CPU if VRAM is tight) that answers chat and voice instantly, hands heavy work to the queue, and can see how things are going (queue, jobs per slot, what is loading), fed by the same data as the dashboard. Check the VRAM budget first: the 27B takes about 17-19GB plus KV cache for the slots. Suggestion, not decided: keep its view read-only *(queued as R1.5a-R1.5b)*

## Phase 4 — Voice / Home Assistant Integration

- [ ] Connect Home Assistant Voice Preview Edition to the Home Assistant instance
- [ ] Confirm on-device openWakeWord detection is working on the Voice Preview Edition
- [ ] Wire wake-word trigger to create/enqueue a task in the RabbitMQ pipeline
- [ ] Route triggered audio to the light model for STT/response
- [ ] Test end-to-end: wake word → light model response → coding queue unaffected

## Phase 5 — Sandboxing & Security

- [ ] Set up Docker + NVIDIA Container Toolkit
- [ ] Run Open Interpreter (or equivalent code-exec layer) inside a locked-down container
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

## Queued jobs — Oct 21, 2026 (index)

Do these in order, one job per round. Each line names its job doc. Read only that job's section there, not the whole doc, and nothing else from this list. J0 makes the queue the source of all work; after J0.7, take jobs from the queue instead of this file. J0.1-J0.8 were done by a Sonnet round on branch `phase-3-queue-agent-jobs`. J0.9 must land on that branch before the migration is run. Run J0, J1 and Q1.2-Q1.3 on Sonnet if the local model runs out of context. Order: C (talk on Discord now, while it works), N (notes for chat and rounds), H (chat schedules coding, does not code), J0 (queue), S (slots: pause, talk, resume), J1 (database), D0 (dashboard fix), E (record everything and show it in the UI, so history starts early), D1-D2 (bot daemon), R (reliable, responsive comms), D3-D8 (commands and chat), Q1.

### C — Talk to Jarvis on Discord now, while it works

- [x] **C1.1 — `jarvis-chat` daemon: instant 👀 and typing, brain reply between round steps** · `docs/jobs/c-chat-now.md`
- [x] **C1.2 — Checkpoint answers don't get a chat reply (`ask-open` marker)** · `docs/jobs/c-chat-now.md`
- [x] **C1.3 — Install `jarvis-chat` as a service** · `docs/jobs/c-chat-now.md`
- [x] **C1.4 — Messages no longer kill the round (only `!pause` does)** · `docs/jobs/c-chat-now.md`
- [ ] **C1.5 — Latency check with Nathan** · `docs/jobs/c-chat-now.md`

### N — Notes: a shared memory for chat and build rounds

- [ ] **N1.1 — Inventory what bootstrap injects (no code)** · `docs/jobs/n-notes.md`
- [ ] **N2.1 — Notes directory and file format** · `docs/jobs/n-notes.md`
- [ ] **N2.2 — Notes package: get, put, delete, list** · `docs/jobs/n-notes.md`
- [ ] **N2.3 — Retrieve a few matching notes, capped at 1,500 words** · `docs/jobs/n-notes.md`
- [ ] **N2.4 — `jarvis-notes` CLI** · `docs/jobs/n-notes.md`
- [ ] **N3.1 — Chat prompt includes retrieved notes** · `docs/jobs/n-notes.md`
- [ ] **N3.2 — Round prompt includes retrieved notes** · `docs/jobs/n-notes.md`
- [ ] **N4.1 — Discord `!note` commands** · `docs/jobs/n-notes.md`
- [ ] **N4.2 — `remember:` / `forget:` and a line in the briefs** · `docs/jobs/n-notes.md`
- [ ] **N5.1 — Seed script (copy, never overwrite)** · `docs/jobs/n-notes.md`
- [ ] **N5.2 — Seed notes from the inventory** · `docs/jobs/n-notes.md`
- [ ] **N6.1 — Restart and live check with Nathan** · `docs/jobs/n-notes.md`
- [ ] **N6.2 — `docs/notes.md` how-to** · `docs/jobs/n-notes.md`

### H — Chat schedules coding work; it does not code

- [ ] **H1.1 — Seed note and chat prompt: you do not code** · `docs/jobs/h-handoff.md`
- [ ] **H1.2 — `jarvis-jobs add` writes an agent job to the store** · `docs/jobs/h-handoff.md`
- [ ] **H2.1 — `!build` and `!fix` enqueue; confirm in Discord** · `docs/jobs/h-handoff.md`
- [ ] **H2.2 — Model `SCHEDULE` block; Go enqueues it** · `docs/jobs/h-handoff.md`
- [ ] **H2.3 — Scheduling does not pause the round** · `docs/jobs/h-handoff.md`
- [ ] **H3.1 — Live check with Nathan (do not start agent-run)** · `docs/jobs/h-handoff.md`
- [ ] **H3.2 — Point D5.1 here; `docs/handoff.md`** · `docs/jobs/h-handoff.md`

### J0 — Make the queue the source of all work

- [x] **J0.1 — Worker stops faking results (keeps output; API jobs failed, not completed)** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.2 — Agent job fields in the message and the enqueue CLI** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.3 — Job store with claims (file-backed, behind an interface)** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.4 — `jarvis-jobs` CLI: next, claim, done, fail, list** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.5 — `agent-run` takes the next job from the queue and names it at the checkpoint** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.6 — Briefs say "do the job you were handed" and follow the job rules** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.7 — Migrate this index and the remaining TASKS.md items onto the queue** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.8 — Dashboard shows agent jobs** · `docs/jobs/j0-queue-work-source.md`
- [x] **J0.9 — Migration reads this index: dotted IDs, job doc sections, ticked lines skipped; rebase onto main** · `docs/jobs/j0-queue-work-source.md`
- [ ] **J0.10 — Put the job rules into the round briefs and the agent-job prompt** · `docs/jobs/j0-queue-work-source.md`
- [ ] **J0.11 — Migration chains jobs only within their group** · `docs/jobs/j0-queue-work-source.md`
- [ ] **J0.12 — `scripts/deploy_queue.sh`: install the queue tools from main, one worker, migration** · `docs/jobs/j0-queue-work-source.md`
- [ ] **J0.13 — Deploy, run the migration, hand the loop over (Sonnet, ask Nathan first)** · `docs/jobs/j0-queue-work-source.md`

### S — Slots: pause the work, talk on Discord, resume

- [ ] **S0.1 — Measure what a pause costs today (no changes)** · `docs/jobs/s-slots.md`
- [ ] **S1.1 — Each round gets a Claude Code session ID** · `docs/jobs/s-slots.md`
- [ ] **S1.2 — Pause stops the round gently (SIGINT, then kill)** · `docs/jobs/s-slots.md`
- [ ] **S1.3 — Resume continues the same session** · `docs/jobs/s-slots.md`
- [ ] **S1.4 — `agent-run` goes straight back to the paused job** · `docs/jobs/s-slots.md`
- [ ] **S1.5 — `jarvis-reply`: a light reply path without Claude Code** · `docs/jobs/s-slots.md`
- [ ] **S1.6 — Live pause and resume test with Nathan** · `docs/jobs/s-slots.md`
- [ ] **S2.1 — Engine facts for llama.cpp and vLLM, from current sources** · `docs/jobs/s-slots.md`
- [ ] **S2.2 — VRAM budget for each engine** · `docs/jobs/s-slots.md`
- [ ] **S2.3 — Recommendation; Nathan picks the engine** · `docs/jobs/s-slots.md`
- [ ] **S3.1 — Install script for the chosen engine (Sonnet)** · `docs/jobs/s-slots.md`
- [ ] **S3.2 — Serve a small model with two slots (Sonnet, brain unloaded)** · `docs/jobs/s-slots.md`
- [ ] **S3.3 — Claude Code's API works through it (Sonnet)** · `docs/jobs/s-slots.md`
- [ ] **S3.4 — Slots keep their cache (Sonnet)** · `docs/jobs/s-slots.md`
- [ ] **S4.1 — Brain weights in the engine's format** · `docs/jobs/s-slots.md`
- [ ] **S4.2 — `engine-switch` with rollback (Sonnet, dry run only)** · `docs/jobs/s-slots.md`
- [ ] **S4.3 — Engine service and the `ENGINE_URL` switch** · `docs/jobs/s-slots.md`
- [ ] **S4.4 — Switch the brain (Sonnet, ask Nathan first)** · `docs/jobs/s-slots.md`
- [ ] **S5.1 — Chat goes to the chat slot, top priority** · `docs/jobs/s-slots.md`
- [ ] **S5.2 — Pause only when there's no room** · `docs/jobs/s-slots.md`
- [ ] **S5.3 — Save and restore the work slot's cache (llama.cpp only)** · `docs/jobs/s-slots.md`
- [ ] **S5.4 — Slot state for the dashboard and Discord** · `docs/jobs/s-slots.md`
- [ ] **S5.5 — Update the R1.5 and Q1 job docs for slots** · `docs/jobs/s-slots.md`
- [ ] **S5.6 — Live check with Nathan, and `docs/slots.md`** · `docs/jobs/s-slots.md`

### J1 — Job records in a database Nathan can query

- [ ] **J1.1 — Install PostgreSQL: `agent` read-write, `nathan` read-only, no passwords** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.2 — Schema migrations: `jobs` and `job_events`** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.3 — PostgreSQL job store with SKIP LOCKED claims** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.4 — Worker, enqueue and `jarvis-jobs` use the database** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.5 — Dashboard reads the database** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.6 — Backfill `jobs.jsonl` and the J0 store** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.7 — Nightly `pg_dump` backups** · `docs/jobs/j1-jobs-database.md`
- [ ] **J1.8 — `docs/database.md` with example queries** · `docs/jobs/j1-jobs-database.md`

### D0 — Dashboard: raw JSON over the UI, and keeping it running

- [ ] **D0.1 — HTML partial for the 5-second poll; remove the body poll** · `docs/jobs/d0-dashboard.md`
- [ ] **D0.2 — Fix the template error that cuts the page off; buffered render** · `docs/jobs/d0-dashboard.md`
- [ ] **D0.3 — Dashboard and worker as systemd services; exactly one worker** · `docs/jobs/d0-dashboard.md`

### E — Everything in the database, shown in the UI

- [ ] **E1.1 — `events` and `rounds` tables** · `docs/jobs/e-activity-ui.md`
- [ ] **E1.2 — `jarvis-event` CLI; `agent-run` records rounds, checkpoints and answers** · `docs/jobs/e-activity-ui.md`
- [ ] **E1.3 — Per-round log files, linked from the database** · `docs/jobs/e-activity-ui.md`
- [ ] **E1.4 — Worker and job state changes as events** · `docs/jobs/e-activity-ui.md`
- [ ] **E1.5 — Every Discord message in and out as events** · `docs/jobs/e-activity-ui.md`
- [ ] **E1.6 — System sampler: GPU, models, queues, services every 30s** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.1 — Dashboard "Now" panel: current round, job, model, VRAM, services** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.2 — Activity timeline page with filters** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.3 — Job detail page: events, output, PR, round log** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.4 — Rounds page with live log tail** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.5 — Discord conversation view** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.6 — Charts: VRAM and power, jobs per hour, duration per model** · `docs/jobs/e-activity-ui.md`
- [ ] **E2.7 — Password on the dashboard** · `docs/jobs/e-activity-ui.md`

### D1-D2 — Discord bot daemon

- [ ] **D1.1 — Bot daemon with owner-only intake** · `docs/jobs/d-discord.md`
- [ ] **D1.2 — `jarvis-bot.service`** · `docs/jobs/d-discord.md`
- [ ] **D1.3 — Optional guild channel and Nathan's manual steps in the docs** · `docs/jobs/d-discord.md`
- [ ] **D2.1 — Daemon routes answers to open `ask` questions over a local socket** · `docs/jobs/d-discord.md`
- [ ] **D2.2 — `jarvis-discord ask` uses the socket, with fallback** · `docs/jobs/d-discord.md`

### R — Reliable, responsive direct comms over Discord

- [ ] **R1.1 — Survive disconnects: catch up missed DMs, dedupe, in order** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.2 — Outbox with retry, 429 handling, no double posts** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.3 — Watchdog and a REST-only "bot down" alert** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.4 — Instant acknowledgement: reaction and typing within 1s** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.5a — Measure fast-chat options while the GPU is busy; Nathan picks** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.5b — Front-desk replies within 10s, brain answers follow** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.6 — Talk to the running round (`>` messages via a Claude Code hook)** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.7 — Progress pushes with `!verbose`** · `docs/jobs/r-discord-reliability.md`
- [ ] **R1.8 — Reliability and latency check with Nathan** · `docs/jobs/r-discord-reliability.md`

### D3-D8 — Discord commands, status, jobs, chat, memory

- [ ] **D3.1 — Command parser and `!help`** · `docs/jobs/d-discord.md`
- [ ] **D3.2 — `!pause jobs` and `!resume jobs`** · `docs/jobs/d-discord.md`
- [ ] **D3.3 — `!pause build` and `!resume build`** · `docs/jobs/d-discord.md`
- [ ] **D3.4 — Confirm buttons for risky commands** · `docs/jobs/d-discord.md`
- [ ] **D4.1 — Shared status package used by the dashboard and the bot** · `docs/jobs/d-discord.md`
- [ ] **D4.2 — `!status`** · `docs/jobs/d-discord.md`
- [ ] **D4.3 — `!queue`, `!jobs`, `!failures`, `!prs`** · `docs/jobs/d-discord.md`
- [ ] **D5.1 — `!add` with source and reply_to** · `docs/jobs/d-discord.md`
- [ ] **D5.2 — Job results sent back to Discord** · `docs/jobs/d-discord.md`
- [ ] **D5.3 — `!cancel`** · `docs/jobs/d-discord.md`
- [ ] **D5.4 — `!move` (reprioritize)** · `docs/jobs/d-discord.md`
- [ ] **D6.1 — Free-form chat goes to the brain as a voice job** · `docs/jobs/d-discord.md`
- [ ] **D6.2 — Status-aware prompt, typing indicator, busy and timeout handling** · `docs/jobs/d-discord.md`
- [ ] **D7.1 — Conversation store with a token-budget window** · `docs/jobs/d-discord.md`
- [ ] **D7.2 — Rolling summary of older turns** · `docs/jobs/d-discord.md`
- [ ] **D7.3 — `!reset`, `!forget`, `!context`** · `docs/jobs/d-discord.md`
- [ ] **D8.1 — `docs/discord-bot.md` and a smoke script** · `docs/jobs/d-discord.md`
- [ ] **D8.2 — Live checklist with Nathan** · `docs/jobs/d-discord.md`

### Q1 — Upgrade the local brain to Qwen 3.8

- [ ] **Q1.1 — Pull `qwen3.8:27b`, keep a 3.6 rollback copy, add the Modelfile (local model is fine)** · `docs/jobs/q1-qwen38.md`
- [ ] **Q1.2 — Smoke test the new brain (Sonnet)** · `docs/jobs/q1-qwen38.md`
- [ ] **Q1.3 — Switch with a tested rollback (Sonnet, ask Nathan first)** · `docs/jobs/q1-qwen38.md`
