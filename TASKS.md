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
