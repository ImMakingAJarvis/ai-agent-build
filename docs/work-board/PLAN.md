# Jarvis Work Board: Plane + Plan with Model

Oct 27, 2026 · @Nathan Meyers

## Goal

Replace the agent's homemade kanban with a self-hosted Plane board, then add **Plan with model** on top of it. Adding a `plan:<model>` label to an epic queues a planning job, the chosen local or cloud model writes a plan and small tasks with acceptance criteria, and once you approve, those tasks run through the queue one at a time and update the board as they go.

How to use this doc: export it as Markdown, save it in `ai-agent-build` as `docs/work-board/PLAN.md`, and give Qwen the task list one item at a time, in order. Each task is sized for one fresh context and one PR. Qwen re-reads this file at the start of every task and appends decisions to `docs/work-board/NOTES.md`.

## Work board: Plane

Plane Community Edition is the board: free, self-hosted in Docker, no user limits, with a REST API and webhooks the agent uses to create and move work. Epics are a paid Plane feature, so this plan uses parent work items instead, which the free edition supports.

| Concept | In Plane (free edition) |
| --- | --- |
| Epic | Parent work item with the `epic` label |
| Task | Sub-work item under the epic |
| Dependency | "Blocked by" relation between sub-items |
| Model selector | Label `plan:<model id>`, generated from the model registry |
| Effort | Optional label `effort:low` / `effort:medium`; default high |
| Plan document | A Plane page linked from the epic, plus a summary comment |
| Agent identity | Bot member "Jarvis" with its own API key; comments prefixed with the model name, e.g. "Qwen:" |

States, in order: Backlog, Planning, Plan review, Plan approved, Ready, In progress, Blocked, Done.

**Model and placement.** The planner picks a model for each task once, at planning time, and records it as a `model:<id>` label you can change on the board. Where it runs is not a model decision: the registry pins each model to a home location (GPU, CPU or iGPU) with an optional fallback, and the worker routes by that rule with no reasoning at dispatch, using live facts like free VRAM and whether the game watchdog has paused the GPU.

A `run:gpu`, `run:cpu` or `run:igpu` label is an override only; you add it when you want something placed differently, such as forcing a job to CPU to keep the GPU free. An override that can't work (the 27B on `run:igpu`) gets a comment and the item moves to Blocked.

Each location has its own queue and worker, so CPU and iGPU jobs, such as audio transcription, run alongside the GPU job instead of waiting behind it.

A small Go service, `plane-bridge`, receives Plane webhooks and turns board changes into queue jobs. Workers write results back through the API. It ignores events made by the Jarvis bot so the two never loop.

Plane runs on the Synology NAS (32 GB RAM) in Container Manager, so the board stays up when the AI box reboots, swaps models or pauses for gaming. `plane-bridge` and the workers stay on the AI box and talk to Plane over the LAN. Because webhooks sent while the AI box is down can be missed, the bridge runs a catch-up sync on every start. The old homemade board is migrated and retired.

## How it works

Planning and execution are separate stages joined by your approval, so a bad plan never turns into code.

&#91;embedded content: epic planning and execution flow\]

Re-plan loops back with your comments. A task that fails its check 3 times is escalated once, then stops and asks you on Discord.

## Model registry

The model selector reads from one registry file, `config/models.yaml`, so adding a new reasoning model is a config change, not a code change. Each entry declares what it is good for, and the planner only offers models tagged `planner`.

In Plane, the selector is a label: `plane-bridge` keeps one `plan:<id>` label per enabled planner model, so enabling a model in the file makes it pickable on the board, and disabling it removes the label.

| Field | Example | Purpose |
| --- | --- | --- |
| id | `qwen3.8-flash-next` | Key used in jobs and logs |
| display\_name | Qwen 3.8 Flash Next (MoE) | Shown in the selector |
| location | local / cloud | Groups the dropdown |
| engine | ollama / llama.cpp / vllm / anthropic / openai-compatible | How the worker calls it |
| endpoint | `http://localhost:8080/v1` | Base URL |
| roles | planner, executor, reviewer | Which buttons offer it |
| context\_tokens | 131072 | Caps what the planner packs in |
| vram\_gb / ram\_gb | 20 / 64 | Lets the worker check fit and decide offload |
| thinking | budget param name + default | Maps the effort setting |
| cost\_per\_mtok | 0 for local | Shown next to cloud picks |
| enabled | true | Hide without deleting |
| home\_location / fallback\_location | gpu / none | Where the worker always runs it, and where it goes if home is unavailable |

Starting entries:

- **Qwen 3.8 27B** (local, dense): executor; planner for small epics.
- **Qwen 3.8 Flash Next MoE** (local, once downloaded): primary local planner. Needs a fit check, since a large MoE will likely run partly from the 128 GB RAM.
- **Claude Sonnet / Opus** (cloud): planner and reviewer for big or stuck epics.
- **Slot for others**: any model the benchmark leaderboard ranks high on reasoning gets added here with the `planner` role.

Loading rule: only one big model fits in VRAM, so picking a local planner that is not loaded becomes a queue job that unloads the current model, loads the planner, plans, then restores the previous model. Planning jobs jump to the front of the background queue, so they start as soon as the current background job finishes. The small always-on front-end model stays up throughout.

## Planning pipeline

Planning runs as three separate model calls, each with a fresh context, and nothing reaches the board until you approve it.

1. **Plan** (chosen model, high effort): input is the epic title, description, and a repo summary (file tree plus README). Output is `PLAN.md` covering goal, architecture, files touched, risks, and open questions for you.
2. **Decompose** (same model, medium effort): turns the plan into a JSON task list. Each task has a title, description, files likely touched, dependencies, an acceptance check (a test name or command that must pass), a size of S or M only, and a suggested executor model. Any task bigger than M gets split again.
3. **Review** (a different model if available, e.g. cloud reviewing local or vice versa): checks for missing steps, tasks too large for one context, and acceptance checks that are not actually runnable. Returns fixes, which get applied once.

The output must validate against a JSON schema. On invalid JSON, retry up to 2 times with the parse error included, then mark the planning job failed and ping you on Discord.

**Approval in Plane:** the plan lands as a Plane page linked from the epic, the draft tasks as sub-items in Plan review, and the epic moves to Plan review. Edit, delete or reorder the sub-items, then move the epic to **Plan approved**, which moves its sub-items to Ready. To **re-plan**, comment on the epic and move it back to Planning; the comments go into the new planning prompt.

## Execution loop

Each Ready sub-item becomes one queue job, and the Go worker runs it with a fresh context and state kept in git, not in the model. Every step writes back to Plane: state changes, a comment with the check result and attempt number, and the PR link on the epic.

1. Pick the next card whose dependencies are all Done.
2. Create branch `epic-<id>/task-<n>` from the epic branch.
3. Build context: `PLAN.md`, `NOTES.md`, the task card, and only the files the card lists.
4. Executor model edits files, using an existing harness (Aider or OpenHands pointed at the local endpoint) instead of a custom edit format.
5. Run the card's acceptance check plus the build and lint.
6. Pass: commit, merge into the epic branch, move the card to Done, append a one-line note to `NOTES.md`.
7. Fail: feed the errors back and retry, up to 3 attempts. After that, escalate once to the planner or a cloud model. Still failing: move the card to Blocked and ask you on Discord.

When every card is Done, open one PR from the epic branch to `main` for your review. A card in progress shows its model, attempt number, runtime and power draw, matching the queue view you already planned. Your conversation queue still takes priority between steps.

## Task list for the agent

Run these in order, one per fresh context, with each ending in a commit and its check passing. Phase 0 stands up the board; everything after builds on it.

**Phase 0: Plane board**

- [ ] P1. Nathan does this one by hand; the agent gets no NAS shell access. Deploy Plane Community Edition on the Synology as a Container Manager project, LAN only, with its data in a dedicated shared folder covered by snapshots or Hyper Backup and set to start after a NAS reboot. Then create an admin account, generate an API key for the Jarvis bot, and add the Plane URL and key to the NAS env file. The agent starts at P2 and only uses the Plane API. Check: the login page loads from the AI box, and an API call with the key from the AI box succeeds.
- [ ] P2. Write an idempotent `plane-setup` script that creates the Jarvis workspace, the `ai-agent-build` project, the states (Backlog, Planning, Plan review, Plan approved, Ready, In progress, Blocked, Done), the `epic` label, and a Jarvis bot member whose API key goes in the NAS env file. Check: a second run changes nothing.
- [ ] P3. Add a Go `plane` client package: create work item and sub-item, set state, add and remove labels, add "blocked by" relations, comment, create a page. Check: tests against a scratch project.
- [ ] P4. Build the `plane-bridge` service: receives Plane webhooks, verifies the webhook secret, ignores events made by the Jarvis bot, and logs each event. Run it as a systemd service on the AI box. On every start it does a catch-up sync through the API and acts on anything left in a trigger state, such as an epic with a plan: label still in Backlog or a sub-item in Ready with no job. Check: editing an item by hand logs an event; a bot edit does not; a label added while the bridge is stopped gets queued when it starts.
- [ ] P5. Migrate open items from the old homemade board into Plane, then switch the old tool off. Check: item counts match; old tool no longer starts on boot.
- [ ] P6. Post a comment on every item the agent touches, prefixed with the model name, and add the board link to the Discord status messages. Check: a test job leaves a "Qwen:" comment.

**Phase A: Model registry**

- [x] A1. Create `config/models.yaml` with the schema above and entries for Qwen 3.8 27B, Qwen 3.8 Flash Next MoE (`enabled: false` until pulled), and Claude Sonnet. Check: a Go unit test loads and validates the file.
- [x] A2. Add a Go `registry` package that loads the file and exposes `ByRole(role)`. Check: tests for filtering by role and skipping disabled models.
- [x] A3. Sync planner models to Plane labels: one `plan:<id>` label per enabled planner model, removed when disabled; runs at bridge start and when the file changes. Check: enabling a model adds its label.
- [ ] A4. Add a VRAM/RAM fit check using `nvidia-smi` and free memory, returning `fits`, `needs_offload` or `too_big` per model; post the result as a comment when a planning job is queued. Check: test with mocked readings.
- [ ] A5. Placement rules: add a `placement` function that takes a model id plus live state (free VRAM, GPU paused by the watchdog, location present) and returns a location from the registry's home and fallback, honoring a `run:` override label when present. Sync one `model:<id>` label per enabled executor model and the `run:` override labels, only for locations the AI box actually has, detected at start. Check: table tests for home, fallback when the GPU is paused, a valid override, and an invalid override returning Blocked.
- [ ] A6. Per-location queues and workers: `jobs.gpu`, `jobs.cpu`, `jobs.igpu`, each with its own Go worker so CPU and iGPU jobs run in parallel with the GPU job. The game watchdog pauses only the GPU queue. Check: a CPU job finishes while a long GPU job is still running.

**Phase B: Plan with model**

- [ ] B1. In the bridge, when a `plan:<id>` label is added to an item labeled `epic`, publish a `plan_epic` job (epic id, model id, effort from an `effort:` label, default high) at the front of the background queue, move the epic to Planning, and comment its queue position. It never preempts a running job, and the conversation queue still comes first. Check: integration test publishes a normal job then a plan\_epic job and sees plan\_epic consumed first.
- [ ] B2. Worker handler: swap in the chosen local model if needed (unload, load, restore afterward), or call the cloud API. Check: test with a stub model.
- [ ] B3. Implement the Plan, Decompose and Review prompts as files in `prompts/`, with the task JSON schema. Check: schema validation test on a sample output.
- [ ] B4. Deliver the plan: create a Plane page with the plan and link it from the epic, create the tasks as sub-items in Plan review with "blocked by" relations and a model: label on each (no run: label; placement comes from the registry), and move the epic to Plan review. Check: a toy epic ends up with a page and sub-items.
- [ ] B5. Approval and re-plan: epic moved to Plan approved moves its sub-items to Ready; epic moved back to Planning re-runs planning with the epic's comments added to the prompt. Check: both paths on the toy epic.

**Phase C: Execution**

- [ ] C1. `run_task` job for each Ready sub-item whose blockers are Done, executed by the model in its model: label and published to the queue the placement function picks (an invalid override goes to Blocked with a comment): branch, context build, harness call, acceptance check, retry up to 3, with state and a result comment written to Plane each step. Check: end-to-end run on a toy epic ("add a /health endpoint").
- [ ] C2. Escalation and Blocked state, with a Discord ping prefixed by the model name and the item link. Check: forced failure reaches Blocked and sends a message.
- [ ] C3. When all sub-items are Done, open the PR to `main` via the GitHub App and post the link on the epic. Check: toy epic produces a PR and a comment.
- [ ] C4. Record per job: model, start, finish, attempts, tokens, power. Check: rows visible in Grafana.

**Phase D: Add bigger reasoning models**

- [ ] D1. Pull Qwen 3.8 Flash Next MoE, run the benchmark job on it, then set `enabled: true` with measured tokens per second. Check: its `plan:` label appears in Plane.
- [ ] D2. Write `docs/adding-a-model.md`: the steps to add any new model (pull, benchmark, registry entry, role tags). Check: you follow it to add one more model.

## Open decisions

- [ ] Which harness for task edits: Aider, OpenHands, or a custom loop in the Go worker?
- [ ] Cloud spend cap per epic, and whether cloud escalation needs your OK each time.
- [ ] Does the Flash Next MoE fit well enough with RAM offload to be worth the swap time, versus planning with the 27B and reviewing in the cloud? Phase D answers this with real numbers.
- [ ] Should the planner be allowed to create sub-epics for very large work, or always stop at tasks?
- [ ] Should the old homemade board's history be migrated too, or only open items?
