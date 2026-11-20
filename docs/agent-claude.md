# The agent's own CLAUDE.md

This is the local agent's personal note file. It is installed to
`~/.claude/CLAUDE.md` by `scripts/install_claude_md.sh`, so Claude Code loads
it in every session, on top of the repo's `CLAUDE.md` (which is about
conversation notes).

## Definition of done — build, verify live, deploy

These rules come from the 2026-10-26 → 11-19 record: most repeat fixes
were work that passed its tests and was called done without being seen
working on the running system, or was merged but never deployed.

1. **Check first that it is not already done.** Search open and merged
   PRs for the job's subject before building. If the work is on main,
   say so with evidence and finish — do not rebuild it.
2. **Read the spec of every service you touch** (`specs/<id>.md`, see
   "Service specs" below) before changing it — it names what the service
   talks to, where it runs, how to deploy it and how to check it.
3. **Build and test.** `go build ./... && go vet ./... && go test ./...`
   for Go; `python3 -m py_compile` for the Python services; `bash -n`
   for scripts; `docker compose config -q` after any compose edit.
4. **Verify live, on the path the owner uses.** Tests are not enough.
   - Code that runs in a container is checked from **inside that
     container** (`docker exec`), not on the host. The host has tools,
     mounts, env and network names the container lacks (procps, ffmpeg,
     the docker socket, `/var/lib/jarvis/*`, `redis` vs `127.0.0.1`).
   - A Discord command is checked by the daemon's own handler path, a
     dashboard change by fetching the page from the LAN address, a
     queue change by a real job going through the real loop.
   - If the check cannot run this round (needs the swap window, a game,
     the owner's ears or phone), the job is **not done**. Put
     `NOT VERIFIED LIVE: <what, why>` as the first line of the PR body
     and the Discord note, and yield or queue the verifying round. Do
     not leave it as an "Open" line in the notes.
5. **Deploy what you changed, from main's code.** A merged PR changes
   nothing until the binary, script or image is installed and the
   process restarted. Use the component's `scripts/install_*.sh`; for a
   compose service `docker compose build <svc> && docker compose up -d
   <svc>` (`docker restart` keeps the old image and the old env). A
   bash loop (`agent-run`) keeps running its old script until the
   service restarts — use `agent-run-restart`. Never install a binary
   built from a branch that is behind `origin/main`: rebase first. Then
   confirm the running thing is the new one (version, build time, or
   the new behaviour) and say so in the PR.
6. **Update the spec in the same PR.** A new service, a new port, env
   var, dependency, data path, deploy step, live check or failure mode
   you hit goes into its `specs/<id>.md` in the same PR as the code.
7. **Report honestly.** A command that failed is a failed job: finish
   with a non-zero code, never 0. Never tell the owner something was
   queued, sent or fixed unless you saw it happen.
8. **Before killing or deleting, look at the exact target.** Kill by
   PID, never `pkill -f`/`pgrep -f … | xargs kill` with a pattern that
   appears in your own job text (your prompt is in your process's
   argv). No globs on `git branch -D`. Never force-push a shared branch.

## Service specs — one per service, kept true

Every service, host unit, CLI and external system has a spec in
`specs/<id>.md`: YAML front matter (group, kind, status, compose
services, units, binaries, ports, health probe, `depends_on` with a
reason) and seven required sections — Purpose, Interfaces,
Configuration, Data, Deploy, Verify live, Failure modes. The standard
and the template are `specs/README.md` and `specs/_template.md`.

- **Enforced:** `go test ./...` (package `./specs`) fails when a compose
  service, a `systemd/` unit or a `cmd/` package has no spec, when a
  spec names something that no longer exists, when a section is missing
  or empty, or when a `depends_on` target is not a spec. A PR that adds
  a service without a spec does not build green.
- **Find the right spec:** `grep -l "<compose name or cmd/pkg>" specs/*.md`,
  or open the System map.
- **Facts only, no secrets.** Leave out what you do not know; name env
  files and variables, never their values.
- **The System map** — `http://111.1.2.27:8080/map`, linked from the
  queue site — is drawn from the specs (embedded in the dashboard
  binary), with live health from each spec's `health` target. It changes
  when the dashboard image is rebuilt.

## Standing rules from the owner

- **No localhost links.** Any link shown to the owner (Discord, the
  dashboard, a PR, a caption) uses the box's LAN address
  (`111.1.2.27`), never `localhost` or `127.0.0.1`.
- **Dates and times** in a UI are rendered with moment in the viewer's
  local timezone, never raw UTC.
- **The brain runs in docker only** (compose service `engine`). There
  is no systemd brain; do not create or start one.
- **A game gets the GPU.** A running Steam game takes the 4090 from the
  brain; a Sunshine desktop stream does not.
- **Cloud models are opt-in.** Local is the default driver. Sonnet,
  Opus and Fable run only when the job spec or the owner names them.
- **A mode change is a switch, not a job.** `!automode`, `!fullauto`,
  `!keepgoing` flip flags; never queue work for them.

## Machine state — the snapshot is in every round prompt

Every round prompt starts with a **MACHINE SNAPSHOT** taken at round start
by `jarvis-stack-status` — the one command that tells you the state of
the machine: docker containers, loaded llama.cpp engines (model, quant,
context, VRAM, health), host RAM/load/uptime/disk, systemd services
(agent-run, jarvis-watchdog, plane-bridge, task-runner), agent-job queue
counts, and the swap-window state. Exit 1 means something is down (the
report still prints). If your round prompt has no snapshot (the running
jarvis-jobs binary predates the injection), run it yourself at the
start of the round.

    jarvis-stack-status            # human readable
    jarvis-stack-status --json     # machine readable

**Treat the snapshot as the current state of the machine.** Do not
re-discover the layout: no `systemctl list-units`, `ss -tlnp`,
`docker ps`, or curl to engine ports just to see what is running. Probe
deeper (`journalctl`, `nvidia-smi`, `ps`) only when the job's evidence
contradicts the snapshot, or while debugging a specific failure.

Stable facts (trust them; re-verify only when a job says otherwise):
- **Brain**: Qwen3.8-27B Q4_K_M (27.3B, 15.3 GiB), llama.cpp in the
  `jarvis-engine` container, port **11434**, 128K ctx. Holds ~18.5 GB of
  the 4090's 24.5 GB at idle → ~6 GB free; 1.5–2.7 GB free while it is
  in a conversation. This engine is serving this conversation — never
  stop or kill it (the round brief explains why and what to do when a
  job needs the 4090 free: requeue it on its spec driver).
- **iGPU helper**: Qwen2.5-3B Q4_K_M (3.4B), llama.cpp in its own
  container `jarvis-engine-igpu` (compose service `engine-igpu`), port
  **11435**, 32K ctx, Vulkan. It stays up through photo swap windows —
  only the brain container stops. Containers reach it by name
  (`http://jarvis-engine-igpu:11435`); the old host unit of the same
  name is disabled (rollback path).
- **Photo generation** needs ~12 GB of free VRAM, so it runs in the
  brain's swap window (runbook `docs/brain-swap.md`, state
  `jarvis-swap status`).
- **Services**: `agent-run` (job loop), `jarvis-watchdog`,
  `plane-bridge` (Plane webhooks), `task-runner` (kanban; dashboard on
  :8080), `jarvis-memory` (:8101). Plane at 111.1.2.222:8088.

## Helper model — small, always-on, on the iGPU

When you need to change your own setup or restart something, ask the
**helper** first: a small always-on model (Qwen2.5-3B Q4_K_M) running on
the Ryzen iGPU (Vulkan, port 11435, container `jarvis-engine-igpu`) — it
costs zero CPU cores. It knows a curated setup brief (services, ports, restart
procedures, key paths — `scripts/jarvis-helper-context.md`) plus a live
stack snapshot, and it answers "not in my context" honestly for anything
else (then read the doc it points at — its pointers can be vague).

    jarvis-helper "how do I restart the voice service?"
    jarvis-helper -s NAME "follow-up"      # multi-turn session
    jarvis-helper --raw "anything"         # no setup context
    jarvis-helper --status                 # health

Expect ~13 s prompt processing + ~13 tok/s decode on the iGPU. If the
engine is down: `docker restart jarvis-engine-igpu` (or `docker compose
restart engine-igpu`). Swapping the model: replace
`/opt/jarvis-igpu/models/jarvis-igpu.gguf` (keep a `.bak`), restart the
container, re-test — the container bind-mounts `/opt/jarvis-igpu`. If the
setup changes substantially, update
`scripts/jarvis-helper-context.md` and re-run `scripts/install_helper.sh`.
Investigation + benchmarks: docs/igpu-model-handler.md.

## Brain swap — photo window (mechanical, do not re-plan)

Photo generation (art :8120 / bench :8121) needs 12 GB free of the 4090's
VRAM; the brain holds ~17.6 GB, so photos run in a swap window: the brain
stops, photo jobs run as code (the `jarvis-photo` executor, no model
round at all), the brain comes back. The
bracket is **automatic**: the resourced window controller (host systemd
service, `172.17.0.1:9104`; EVB-27, 2026-11-11) opens the window when a
`photo`-tagged job is ready and closes it — brain back — as soon as the
ready batch drains (docs/agent-run.md, "Photo window"). Runbook:
`docs/brain-swap.md`. State: `jarvis-swap status`.

When photo work needs the GPU, in your own round:
1. `jarvis-swap handoff`, fill "In flight" + "First thing on return"
   (2-3 lines each — resume, don't re-plan).
2. Make sure the photo work is queued with the `photo` tag (image-queue
   ingestion sets it; otherwise `jarvis-jobs add ... -tags photo
   -prompt "..."` — the executor reads the prompt from the job's
   sidecar).
3. End the round — the window controller opens it on its next tick (30s).
4. Next brain round after the window: read `notes/brain-handoff.md`, do
   "First thing on return", verify the photos, `mkdir -p notes/archive &&
   mv notes/brain-handoff.md notes/archive/`, jarvis-notes done.

Hard rules: never run `jarvis-swap out`/`in` from a local (brain) round —
the window controller runs them between rounds; a manual run is an igpu
round or a terminal `--force`. Never stop the brain mid-round. The watchdog maintain
window re-arms the brain at the deadline no matter what; `jarvis-jobs
next` skips jobs whose engine is down, so local jobs wait through the
window (photo jobs are exempt — they run as code, gated only by the
swap window itself).

## Think in events — submit what you cannot do in-round

The capability workers (`cap.*` queues) do the work that needs a model or
a device that is not you: image generation, transcription, VLM
description, batch LLM sub-tasks, benchmarks. The round preamble carries
the catalog (`jarvis-mq caps`): one line per capability — where it runs,
the cost, queue depth, typical runtime. Full mechanics:
docs/event-driven.md.

**When to submit vs do it yourself.** If the work fits in this round on
your own context (minutes of your time, no other model or device needed),
do it. If it needs a capability — the catalog names it — submit it: the
worker is purpose-built, and the result comes back to you with its
artifacts.

**The 4-command pattern** (one round, this order — listen first, so a fast
result can never outrun its listener):
1. `jarvis-mq listen flow.$JARVIS_JOB_ID --wake $JARVIS_JOB_ID` — the
   switchboard wakes this job when a Result lands (no `--corr` = claims
   every message on the queue).
2. `jarvis-mq submit <cap> --reply-to flow.$JARVIS_JOB_ID --corr <id>
   [--input k=v ...] [--input-ref P ...] [--out-dir D]` — one short corr
   id per request (`img1`, `tr-1`).
3. `jarvis-jobs yield $JARVIS_JOB_ID --wait-on <id> --note "..."
   --in-flight "..." --next "..."` — the continuation is the precondition
   (invariant 8): the CLI reads it back and refuses a yield whose
   "In flight" or "Next steps" is empty.
4. End the round. The job sits in `waiting`; the switchboard wakes it when
   every `--wait-on` corr is recorded (deadline default 2h — a timeout
   wake marks its results PARTIAL in the prompt).

**Resume** (the woken round): the prompt carries a CONTINUATION block —
read it, then `jarvis-mq results flow.$JARVIS_JOB_ID` (the full Result
messages: payloads, artifacts, errors), verify the artifacts exist and are
sane, do "Next steps", continue the job. Resume, don't re-plan.

**Cost model** (the preamble repeats it): a `needs brain out` capability
runs in your swap window — the window bracket (brain out → back) costs
~2 min and is shared, so if you will need several GPU capabilities, submit
them ALL in this round before yielding: one window, one wake. A capability
that "runs alongside you" (cpu / igpu / spare VRAM) costs no window —
prefer it when it is good enough.

**Fan-out**: N independent sub-tasks = N submits with N corr ids in one
round, then `yield --wait-on c1,c2,...` on all of them — one park, one
wake, every result in the same round.

**Hard rules**: never stop the brain yourself (invariant 1) — the swap
window is opened and closed by the stack (the resourced window controller)
between rounds, and it never opens while your round is running; never yield
without a filled continuation; a Result is not done until you have read it
and verified its artifacts.

## Reviewer flow — get a finished PR reviewed (EVB-36)

When a round finishes a PR and wants a second pair of eyes from another
model, hand it to a reviewer and park (plan §3.8). One command:

    jarvis-review request <PR> [--reviewer sonnet|planner|local] [--board REF] \
        [--summary "what the PR does"]

It posts a `review_request` to the reviewer's inbox (with a wake on the
answer), enqueues the reviewer job (it jumps the queue — class fix,
priority 100), and prints the corr + the yield command. Reviewer rule:
`sonnet` for code PRs (it can run the go gate), `planner` (opus) for
docs/plan-only PRs; default sonnet. `local` is the fallback when the API
drivers are dead (e.g. a bad ANTHROPIC_API_KEY): a fresh local brain
session reviews (same model as the author — it proves the loop, not a
cross-model opinion). Then:

    jarvis-jobs yield $JARVIS_JOB_ID --wait-on <corr> \
        --note "..." --in-flight "PR #N open, review requested" \
        --next "read the review (jarvis-mq inbox brain, then take), address
                the findings, gh-pr-wait N exit 0, notify, finish"

…and end the round. The reviewer (a fresh sonnet/opus session) sees the
request in its own round prompt (the `MAILBOX` section — every round now
peeks its identity's inbox: local→brain, igpu→igpu-helper, sonnet→sonnet,
opus→planner), reviews the PR in a throwaway worktree, and answers with
`approve` / `request_changes` + findings. The answer lands on your inbox
and wakes you; the woken round reads it (`jarvis-mq inbox brain`), fixes
the findings, and finishes the normal PR workflow. Both the request and
the review mirror to the `--board` Plane item. Full mechanics:
docs/event-driven.md, "Reviewer flow".

## The vector graph — more data than the repo

The **Jarvis Memory Service** (compose container `jarvis-memory`,
http://localhost:8101) is a local vector database that indexes the work logs
this agent produces. It holds data that is NOT in the repo working tree:
every past job's spec, state file and transcript, the pending queue, and a
snapshot of the repo docs. Fully local (SQLite + all-MiniLM-L6-v2 ONNX
embeddings).

**Search it before grepping the filesystem** for "what did we do about X",
"what happened in job X", or "what is queued next":

    curl -s -X POST localhost:8101/search -H 'Content-Type: application/json' \
      -d '{"query":"wake word latency","k":5}'

Endpoints:
- `POST /search` — `{"query": "...", "k": 5, "source": "job|jobs|repo-doc|queue"}`
  → top-k chunks, each with `source`, `path`, `title`, `meta`, `score`.
- `GET /stats` — doc/chunk counts per source and last reindex time.
- `GET /health` — service and embedding-model status.

It reindexes automatically every 15 minutes (compose container
`jarvis-memory-reindex`; was the `jarvis-memory-reindex.timer` unit).
Data lives in `~/.local/share/jarvis/memory/memory.db`.

It can also be driven as a CLI
(`python3 scripts/memory_service.py --search "..." | --stats | --reindex`).
If the service is down, run `docker compose restart memory`.

## Secrets — write-only, for keys Nathan gives the agent

Nathan stores secrets (API keys, tokens, passwords) in the dashboard's
**Secrets** page (`/secrets` on :8080, linked from the dashboard shell). It
is write-only by design: he can add and remove a secret and see its name,
size and update time, but no endpoint — dashboard or otherwise — returns
the value. To change a secret he removes it and re-adds it under the same
name.

Storage is one file per secret under `~/.local/share/jarvis/secrets/`
(dir 0700, files 0600, atomic writes; Go store in `internal/secrets`).
The dashboard container and this host both see that directory.

The agent reads them with the local CLI (values from stdin, never argv):

    jarvis-secrets list                 # names only
    echo "the-value" | jarvis-secrets add some-key
    jarvis-secrets get some-key         # value to stdout
    jarvis-secrets rm some-key

`get` is the only read path and it is local to this machine — do not wire
it into anything that runs as another user or exposes a port.

## Keep notes — things done, the system, what Nathan asked

Besides the per-run conversation notes (`notes/conversations/agent-<date>.md`),
keep a small standing record in three buckets under `notes/agent/`, using
`jarvis-notes`:

- `done`   — a curated "shipped" log (one line per finished job/PR)
- `system` — durable facts about this machine/stack (services, ports, gotchas)
- `asked`  — themes of what Nathan has asked about / decided

At the end of each job, record what is worth keeping:

    jarvis-notes add done "<what shipped> (PR #n)" --job <id>
    jarvis-notes add system "<new durable fact, if any>"
    jarvis-notes add asked "<something Nathan asked or decided, if any>"

Each `add` writes the file AND pushes it into the vector graph immediately, so
a note is searchable this session rather than after the 15-minute reindex.
Search the graph (above) before grepping the filesystem for prior work.
See `docs/agent-notes.md` for the full how-it-works.

### Never commit note files in a job's PR (prevents merge conflicts)

The note files — `notes/agent/done.md`, `notes/agent/system.md`,
`notes/agent/asked.md`, and the per-day `notes/conversations/agent-<date>.md`
— are shared and append-only, and nearly every job appends to them. If each
job's branch commits its note lines, any two unmerged PRs append different
lines at the same tail and conflict on merge. That was the recurring
"PRs are docs/notes and they conflict" problem (Nathan, 2026-11-08).

None of it needs a commit to work: the Stop hook only checks that the note
file was *modified* in the working tree, and the vector graph (`jarvis-notes`
+ the 15-minute reindex) reads the working-tree file directly. So:

- Keep writing notes to the working tree exactly as above. Do NOT
  `git add` / commit any `notes/**` file into a job's feature branch. A
  job PR contains only the job's actual work (code + its own docs).
- Persist notes to git separately, in a small **notes-only** commit on a
  branch rebased onto the latest `origin/main`, merged promptly. Keep at
  most one notes-only PR open; if one already is, rebase/update it instead
  of opening a second. That PR is the only one that touches notes, so it
  conflicts rarely and resolves trivially (keep both sides, chronological).
- If you catch that a note file is staged in a job branch, `git restore
  --staged` it before committing.

## Home Assistant (the house)

HA runs at `http://111.1.2.200/` (HA 2026.10 behind a reverse proxy on port
80; 8123 is not reachable from this box).

**Use the `homeassistant` MCP server for all HA tasks.** It is registered in
user scope (`~/.claude.json`), so in every Claude session its tools are
available as `mcp__homeassistant__*` — `GetLiveContext` (current entity
states), light/climate/cover/lock control, media players, vacuum, to-do list,
and the GE/SmartHQ appliance tools. If the tools are missing or `claude mcp
list` shows it down, re-register:

    claude mcp add --transport http --scope user homeassistant http://111.1.2.200/api/mcp \
      -H "Authorization: Bearer $(sudo cat /home/nathan/HAtoken.txt)"

Credentials (never print or commit them):
- Long-lived API token: `/home/nathan/HAtoken.txt` — read with `sudo cat`.
- HA UI login (user `homeai`): `/home/nathan/homeassistantuserpw.txt` — sudo.

**REST fallback** for things the MCP tools don't cover (logbook, events, raw
service calls):

    TOK=$(sudo cat /home/nathan/HAtoken.txt)
    curl -s -H "Authorization: Bearer $TOK" http://111.1.2.200/api/states
    curl -s -H "Authorization: Bearer $TOK" -X POST \
      http://111.1.2.200/api/services/light/turn_on \
      -H 'Content-Type: application/json' -d '{"entity_id":"light.kitchen"}'

WebSocket API (logbook/events): `ws://111.1.2.200/api/websocket`, auth with
`{"type":"auth","access_token":...}` after the hello message.

Caveats: the working MCP endpoint is `/api/mcp` (streamable HTTP) — the
old `/api/mcp_server/sse` path 404s on this HA version. The Jarvis headline
sensors published via MQTT are documented in `ha/README.md`.

## Before you call a PR done

A PR is not done while it conflicts with main. Before finishing any
job that opens or updates a PR, verify the branch has no merge
conflict:

- `gh-pr-wait <n>` (polls every 15 s, so the waiting happens outside
  your context) must exit **0** = open and `MERGEABLE`. Exit **1** =
  `CONFLICTING` — fix it first; exit **2** = merged or closed, nothing
  left to do; exit **3** = timed out while GitHub was still computing
  (re-run it).
- If it conflicts: merge `origin/main` into the branch (in the
  branch's worktree), resolve, push, and re-run `gh-pr-wait`.

The usual conflict is `notes/conversations/agent-<date>.md` — every
round appends to it. Resolve by keeping ALL entries from both sides
(chronological order), never by dropping one side.

Then — and only then — Discord-notify with the PR link and finish the
job done. (Job chat-1793154320-3d8d, 2026-10-27: PR #83 was left
CONFLICTING and had to be reworked.)

## PR review

- Reviews go to `review.sonnet`, `review.opus`, `review.fable`, or
  `review.local`. They do not use class `fix` or priority 100.
- Fable runs only after `!review-release <pr>`.
- The reviewer may commit on the pull request branch when it holds the
  token. It does not open a second pull request. It does not merge to
  `main` unless its id is in `config/review.yaml` `merge_allow`.
- Comments go on the Plane item, posted with that model's secret. They
  do not go on GitHub.
- A revival line in the review log is a bug. Do not delete it to make
  the evening digest quiet.
