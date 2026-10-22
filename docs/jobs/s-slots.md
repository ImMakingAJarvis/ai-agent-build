# S — Slots: pause the work, talk to Nathan, resume

Read only the section for your job. Background, shared by all S jobs:

- Goal (Nathan, Oct 21): the brain keeps a build job going, can stop to answer Nathan on Discord, then carries on from where it was. This implements the "Phase 3b — Inference engine & slots" plan in TASKS.md; read its goal paragraph if you need the why, not the rest of the file.
- Today: Ollama 0.40.7 serves `jarvis-brain` (qwen3.6:27b Q4_K_M, about 17 GB of weights, `num_ctx` 65536, one slot). Loaded, it uses about 23.4 GB of the 4090's 24.5 GB (`docs/gpu-power-measurements.md`). So nothing else fits next to it on the GPU, and the Phase 3b note that a small test model "fits next to the loaded brain" is wrong.
- Claude Code reaches the brain through `scripts/agent-local`, which points `ANTHROPIC_BASE_URL` at Ollama. Any new engine must speak the Anthropic Messages API (`/v1/messages`, with tool use and streaming), or sit behind a proxy that does.
- Pausing has two layers. (1) The agent's conversation: since PR #7 a Discord message kills the round (`SignalFile` in `internal/agentjob/run.go`), `agent-run` answers through a fresh Claude Code run, then requeues the job, which then starts over from nothing. S1 fixes this on any engine by resuming the Claude Code session. (2) The engine's KV cache: with one slot, any other request (the chat reply, or Claude Code's own small haiku-tier calls, which `agent-local` also maps to the brain) evicts the round's cached prompt, and the next call recomputes it. S0 measures what that costs before anyone switches engines.
- Engines with slots: llama.cpp `llama-server` (`-np`, `--slot-save-path`, `/slots/<id>?action=save|restore`; reads GGUF) and vLLM (paged KV cache, prefix caching, preemption; needs AWQ/GPTQ-style weights rather than GGUF). Verify every feature against current docs and releases yourself (S2.1). qwen3.6 may use linear-attention layers, which change what "saving the cache" means.
- Only one engine can hold the 27B in VRAM at a time. Rounds that start, stop or load models on the GPU (S3, S4) run on Sonnet with the brain unloaded, because a local round needs the brain the whole time.
- Q1 (Qwen 3.8 upgrade) assumes Ollama. S5.5 updates its doc if the engine changes.

## S0.1 — Measure what a pause costs today (no changes)

- Files: new `scripts/measure_prefill.sh`, new `docs/slots-baseline.md`.
- Against Ollama's `/api/chat` (`jarvis-brain`, `stream: false`), send prompts of about 10K, 30K and 50K tokens twice in a row, then once more after a short unrelated request. Record `prompt_eval_count` and `prompt_eval_duration` for each call: first, cached, and after eviction.
- Also count the brain calls in one real round: grep the last round in `agent.log` (or Ollama's journal with `journalctl -u ollama --since`) for requests, and note how many are short side calls.
- Done when: `docs/slots-baseline.md` has a table of seconds lost per pause at each prompt size, plus the side-call count. Keep the script under 80 lines.

## S1.1 — Each round gets a Claude Code session ID

- Files: `internal/agentjob/run.go`, `internal/agentjob/store.go`, and their test.
- When a job starts fresh, make a UUID, store it on the job (`session_id`), and pass `--session-id <uuid>` to `claude`/`agent-local` (both pass extra arguments through). `jarvis-jobs show <id>` prints it.
- Done when: a test with a fake command sees `--session-id` with the stored UUID, and `go test ./...` passes.

## S1.2 — Pause stops the round gently

- File: `internal/agentjob/run.go` and its test.
- When the signal file appears, send SIGINT to the agent process, wait up to 20 s, then kill. Record `paused_at` and the last 2 KB of output on the job. Claude Code then has time to save its session transcript.
- Done when: a test with a fake command that traps SIGINT shows it got SIGINT first, and the job is `paused` with `paused_at` set.

## S1.3 — Resume continues the same session

- File: `internal/agentjob/run.go` and its test.
- When a `paused` job with a `session_id` runs again, call the agent with `--resume <session_id>` and the prompt: "You were paused so I could talk to Nathan. Continue job <ID> — <title> where you left off. Run `git status` and check the branch first." If `claude` reports the session is missing, fall back to a fresh start with a new ID and say so in the job's summary.
- Done when: tests cover the resume arguments and the fallback.

## S1.4 — `agent-run` goes straight back to the paused job

- File: `scripts/agent-run` only (the paused branch near `jarvis-jobs requeue`).
- After the reply to Nathan, post "Back to <ID> — <title>" and run the job again at once, with no checkpoint, unless Nathan's message was `stop`, `skip` or `wait` (then requeue and go to the checkpoint as today).
- Install atomically (copy to a temp name in `/usr/local/bin`, then `mv`), because a running `agent-run` reads its script as it goes.
- Done when: `bash -n` passes and a dry run with `AGENT_RUN_FAKE=1` pauses, replies and resumes one job.

## S1.5 — A light reply path that doesn't start Claude Code

- Files: new `cmd/jarvis-reply/main.go` and its test; one line in `scripts/agent-run` to use it.
- Today the reply to Nathan starts a full Claude Code session with tools, which is slow and pulls a big prompt through the brain. Replace it with a single chat call: a short system prompt, the job's ID, title and last 20 output lines, and Nathan's message. Send the answer with `jarvis-discord notify`. Time limit 2 minutes; on timeout, say "I'm busy on <job>, I'll answer after it" and go on.
- The engine URL and model come from `local.env` (`OLLAMA_URL`, `LOCAL_MODEL`), so S4 only changes the URL.
- Done when: a test against a fake HTTP server checks the request and the timeout message.

## S1.6 — Live test with Nathan

- Ask Nathan with `jarvis-discord ask -choices "Ready,Later"`. During a local round, he sends three messages a few minutes apart.
- For each, record the time to the reply, the time until the round is generating again, and whether the round carried on, rather than redoing work.
- Done when: the results are in `docs/slots-baseline.md` and Nathan has confirmed on Discord that resume works.

## S2.1 — Engine facts, checked against current sources (no installs)

- File: new `docs/engine-choice.md`.
- For the current releases of llama.cpp server and vLLM, check from their docs and release notes, with links and versions: support for this model's architecture, the 4-bit formats available for it and their size, Anthropic `/v1/messages` (else name a proxy such as LiteLLM and what it costs), several slots or concurrent requests, request priority, saving and restoring one request's cache, prefix caching, MTP or draft decoding, and install method (venv, Docker, build).
- Done when: the doc has one table with a source link per cell. No opinions yet.

## S2.2 — VRAM budget for each engine

- File: `docs/engine-choice.md` only.
- For each engine, add up the weights, a work slot at 64K, a chat slot at 8K, and runtime overhead, against 24.5 GB. Start from today's measured 23.4 GB. Say whether two slots fit, and if not, what context the work slot can keep.
- Done when: the doc has the budget table and a one-line answer per engine.

## S2.3 — Recommendation; Nathan picks

- Send Nathan a summary of five lines or fewer: S0.1's seconds lost per pause, what each engine gives, the VRAM verdict and your recommendation. Use `jarvis-discord ask -choices "llama.cpp,vLLM,Stay on Ollama"`.
- Write his pick at the top of `docs/engine-choice.md`. If he picks "Stay on Ollama", mark S3.1 through S5.5 done with `jarvis-jobs finish <id> 0`, not `skip`, because a skipped job blocks the jobs after it. Write why in the doc. S5.6 still runs.
- Done when: the pick is in the doc.

## S3.1 — Install script for the chosen engine (Sonnet)

- File: new `scripts/install_engine.sh`.
- Install the engine Nathan picked at a pinned version under `/opt/jarvis-engine`: a venv for vLLM, or a CUDA build for llama.cpp. No service and no model yet.
- Done when: the script is idempotent, `bash -n` passes, and the engine's `--version` (or `--help`) runs.

## S3.2 — Serve a small model with two slots (Sonnet, brain unloaded)

- File: new `scripts/engine-try.sh`.
- Only between rounds: check that no round is running, then unload the brain (`ollama stop jarvis-brain`). Serve a model of 4B or smaller on port 8090 with two slots at 8K each, then stop it and reload nothing. Ollama reloads the brain on its next request.
- Done when: two concurrent `curl` requests both stream, and the script leaves the GPU as it found it.

## S3.3 — Claude Code's API works through it (Sonnet)

- File: `scripts/engine-try.sh` only.
- Add a check: a `/v1/messages` request (direct, or through the proxy named in S2.1) with one tool definition returns a `tool_use` block, both streaming and not.
- Done when: the check passes against the small model.

## S3.4 — Slots keep their cache (Sonnet)

- Files: `scripts/engine-try.sh`, `docs/engine-choice.md`.
- Send a 6K-token prompt to slot 1, a chat to slot 0, then the 6K prompt to slot 1 again, and time both slot-1 calls. For llama.cpp, also save slot 1 to disk, overwrite it, restore it, and time the restore.
- Done when: the timings are in the doc and show the cache surviving the chat (or say clearly that it doesn't).

## S4.1 — Brain weights in the engine's format

- Files: `scripts/install_engine.sh` (a `--model` step), `docs/engine-choice.md`.
- llama.cpp: reuse the GGUF Ollama already has (find the blob from `ollama show --modelfile jarvis-brain`) or download the same quant. vLLM: download the 4-bit build chosen in S2.1. Check the size against S2.2. Don't serve it yet.
- Done when: the file is in `/opt/jarvis-engine/models` and its size and checksum are in the doc.

## S4.2 — `engine-switch` with rollback (Sonnet, dry run only)

- File: new `scripts/engine-switch`.
- `engine-switch to-engine`: unload the brain from Ollama and stop `ollama.service`, start the engine with a work slot (64K, or S2.2's figure) and a chat slot, wait for health, then run S3.3's check. On any failure, roll back. `engine-switch to-ollama`: stop the engine and start `ollama.service`. Both log to `/var/log/jarvis-engine-switch.log`.
- Done when: `bash -n` passes and `--dry-run` prints every step for both directions. Do not run it for real.

## S4.3 — Engine service and the URL switch

- Files: new `systemd/jarvis-engine.service` (installed but not enabled), `scripts/agent-local`.
- `agent-local` uses `ENGINE_URL` from `local.env` when it is set, else `OLLAMA_URL`. `jarvis-reply` (S1.5) reads the same key.
- Done when: `agent-local check` still passes on Ollama with `ENGINE_URL` unset.

## S4.4 — Switch the brain (Sonnet, ask Nathan first)

- Ask with `jarvis-discord ask -choices "Switch now,Later"`. Announce on Discord that the brain will be offline for a few minutes.
- Run `engine-switch to-engine`, set `ENGINE_URL` in `local.env`, run `agent-local check`, then one tiny local round (for example S5.4's doc-only part). On failure: `engine-switch to-ollama`, unset `ENGINE_URL`, and report. Keep Ollama installed as the rollback.
- Done when: a local round finished on the new engine, and Nathan was told the result.

## S5.1 — Chat goes to the chat slot, top priority

- Files: `cmd/jarvis-reply/main.go` and its test.
- Send replies to the reserved chat slot (llama.cpp: `id_slot`; vLLM: the request `priority` field, with the server's priority scheduling on). Then the round can keep running while Nathan gets an answer.
- Done when: a test checks the slot or priority field, and a live reply arrives while a round is generating.

## S5.2 — Pause only when there's no room

- Files: `scripts/agent-run` (the watcher part), `internal/agentjob/run.go`.
- On a Discord message, ask the engine whether the chat slot is free (llama.cpp `/slots`, vLLM `/metrics`). If it is, reply through `jarvis-reply` without pausing the round. If not, pause as in S1.2-S1.4.
- Done when: a fake-engine test covers both paths.

## S5.3 — Save and restore the work slot's cache (llama.cpp only)

- File: `internal/agentjob/run.go` and its test.
- With llama.cpp: on pause, save the work slot to `/var/lib/jarvis/slots/<job>.bin`, and restore it before resume. With vLLM: mark this job done with the note "vLLM swaps the cache itself".
- Done when: a fake-engine test sees save before the pause and restore before the resume.

## S5.4 — Slot state for the dashboard and Discord

- Files: new `internal/slots/slots.go` (reads the engine's slot or metrics endpoint) and its test; one field in the dashboard's state.
- Show each slot's job, tokens in the cache, and busy or idle. Phase 6 already lists "Show slot state", and D4.1 reuses this package.
- Done when: a test with a fixture response passes and the dashboard shows the slots.

## S5.5 — Update the R1.5 and Q1 job docs for slots

- Files: `docs/jobs/r-discord-reliability.md` (the R1.5a and R1.5b sections only), `docs/jobs/q1-qwen38.md`.
- R1.5a measures the chat slot as the fast path, instead of a CPU model. Q1 pulls and tests the new model in the engine's format, through `engine-switch`.
- Done when: the diff touches only those sections.

## S5.6 — Live check with Nathan, and how-to

- File: new `docs/slots.md`: how slots, pause and resume work, and how to roll back to Ollama.
- During a local round, Nathan sends 10 Discord messages. Record the time to each reply, and whether the round kept going.
- Done when: the numbers are in `docs/slots.md`, and Nathan confirmed on Discord.
