# N — Notes: a shared memory for chat and build rounds

Read only the section for your job. Background, shared by all N jobs:

- Goal (Nathan, Oct 22): Discord chat (`jarvis-chat`) has no picture of this system. Build rounds do, from the stage-3 brief. He wants a notes store the AI can use for system knowledge and anything else it should remember, shared by chat and rounds, that survives restarts. Not in-memory context.
- Today: `cmd/jarvis-chat/main.go` `buildMessages` uses a one-line system prompt plus a status line (`$CONF/current-job` and the last 3 lines of `agent.log`) and the last 10 turns in RAM. Rounds get `STAGE3L` / `STAGE3S` from `bootstrap.sh` (written to `~/.config/jarvis/stage3-*.txt`) plus the job text. Those two paths never share facts.
- D7 (later) is conversation history in the J1 database. Notes are durable facts, not chat turns. Keep them separate. J1 may later store notes in Postgres; keep the store behind a small Go interface so only the implementation swaps.
- Where notes live: `/var/lib/jarvis/notes/` on disk. One Markdown file per note, `0644`, directory `0755`, owned by `agent`, so Nathan can read them. An `index.md` lists id, title, tags. Never put secrets in notes (tokens, `.pem`, env files).
- Retrieval, not dump: pick a few notes that match the message, capped at about 1,500 words, so the 64K window stays intact. `num_ctx` must not change (Ollama would reload the 17 GB model).
- Code to reuse: `cmd/jarvis-chat/main.go` (`buildMessages`), `internal/agentjob/run.go` (`Prompt`), `bootstrap.sh` (`STAGE3L` / `STAGE3S` heredocs — grep, do not read the whole file).

## N1.1 — Inventory what bootstrap injects (no code)

- File: new `docs/notes-sources.md` only.
- Grep `bootstrap.sh` for `STAGE3L`, `STAGE3S`, `agent_write`, and `JCONF`. List `~/.config/jarvis/*.txt` and `*.md` by name (do not open `*.env`). List `docs/*.md` titles. Note that `TASKS.md` is history and too big to inject.
- Table: path, who reads it today (chat / round / both / neither), approximate size.
- Done when: the doc has that table and a one-paragraph gap: chat does not see the brief.

## N2.1 — Notes directory and file format

- Files: new `scripts/setup_notes_dir.sh`, new `docs/notes/README.md` (format only, under 40 lines).
- Create `/var/lib/jarvis/notes` (`agent` owns it, `0755`) and `/var/lib/jarvis/notes/index.md`. Each note is `<id>.md` with YAML front matter `id`, `title`, `tags`, `updated`, then a short body. `id` is `[a-z0-9-]{1,40}`.
- Done when: the script is idempotent, `bash -n` passes, and a sample note Nathan can `cat` as himself.

## N2.2 — Notes package: get, put, delete, list

- Files: new `internal/notes/notes.go` and its test only.
- Interface `Store` with `List() []Meta`, `Get(id)`, `Put(Note)`, `Delete(id)`. File implementation on a directory (temp dir in tests). `Put` is atomic (write to a temp file in the same dir, then `rename`) and refreshes `index.md`. Reject empty bodies and illegal ids.
- Done when: tests cover round-trip, delete, illegal id, and a second `Put` on the same id replacing the body. `go test ./internal/notes` passes.

## N2.3 — Retrieve a few matching notes

- Files: `internal/notes/search.go` and its test only.
- `Search(query, maxNotes, maxWords)` scores title, tags, then body (word overlap, case-insensitive). Return the top notes that fit `maxWords` (default 4 notes, 1,500 words). Empty query returns the notes tagged `core`, still under the cap.
- Done when: tests use a fixture of 8 notes and check that a query about the GPU does not return a note about Discord commands, and that the word cap is held.

## N2.4 — `jarvis-notes` CLI

- File: new `cmd/jarvis-notes/main.go` and its test.
- Commands: `list`, `show <id>`, `add <id> <title> --tags a,b` (body on stdin), `edit <id>` (body on stdin, keeps title unless `-title` is set), `forget <id>`, `search <query>`. Default dir `/var/lib/jarvis/notes`, override with `JARVIS_NOTES_DIR`.
- Done when: CLI tests against a temp dir cover add, search, forget, and `go test ./cmd/jarvis-notes` passes.

## N3.1 — Chat prompt includes retrieved notes

- File: `cmd/jarvis-chat/main.go` `buildMessages` only, plus its test.
- Before calling the brain, `Search` the owner's message (defaults above). Append a `Notes:` block to the system prompt. If none match, append nothing. Do not send the index, and do not change Ollama `options`.
- Done when: a test with a fake store shows the matching note in the system prompt and a miss leaving it unchanged.

## N3.2 — Round prompt includes retrieved notes

- File: `internal/agentjob/run.go` `Prompt` only, plus its test.
- `Search` the job title plus the first 400 characters of instructions. Same `Notes:` block as N3.1. Missing notes dir is a no-op, not an error.
- Done when: a test with a fake store shows the block, and a missing dir still returns a prompt.

## N4.1 — Discord `!note` commands

- File: `cmd/jarvis-chat/main.go` only (command branch before the brain call) and its test.
- `!note list`, `!note show <id>`, `!note add <id> <title>` (rest of the message, or the next line, is the body), `!note edit <id>` (same), `!note forget <id>`, `!note search <query>`. Owner-only, same as chat. Reply with the CLI output, trimmed to 1,900 characters. Do not call the brain for these.
- Done when: tests cover add, show, forget, and unknown-id, with no Chat call.

## N4.2 — "remember" / "forget" in chat, and a line in the briefs

- Files: `cmd/jarvis-chat/main.go` (a few lines), `bootstrap.sh` (`STAGE3L` and `STAGE3S` heredocs only; grep for them), live `~/.config/jarvis/stage3-local.txt` and `stage3-sonnet.txt` (replace atomically).
- If a DM starts with `remember:` or `forget:`, treat it as `!note add` (id from the first few words) or `!note forget` / search-and-forget. Also add one sentence to both briefs and to the chat system prompt: "When you learn a durable fact, write a note with `jarvis-notes add` (or tell me `remember: …`). Do not store secrets."
- Done when: `bash -n bootstrap.sh` passes, and tests cover `remember:` and `forget:`.

## N5.1 — Seed script

- File: new `scripts/seed_notes.sh`.
- Copy `docs/notes/seed/*.md` into `/var/lib/jarvis/notes/` if that id is not already present (never overwrite a note someone edited). Refresh `index.md`. Idempotent.
- Done when: a dry run against a temp dir copies fixtures once and a second run copies nothing.

## N5.2 — Seed notes from the inventory

- Files: new files under `docs/notes/seed/` only (one fact per file, each body under 120 words).
- From N1.1's table, write notes tagged `core` or `system`: hardware (4090 24 GB, 128 GB RAM, Ubuntu 24.04, board ROG STRIX B850-G), who Nathan is, how chat vs a build round works, Ollama traps (`options` reloads the model; one request at a time), dashboard `:8080` / shim `:8081` / Grafana `:3000`, queue (RabbitMQ, `jarvis-jobs`, store still empty until J0.12–J0.13), Discord (owner DM, `jarvis-chat` vs `ask` checkpoints), git (org `ImMakingAJarvis`, never push to main, `GH_TOKEN=$(ghapp-token) gh …`).
- Then run `scripts/seed_notes.sh`.
- Done when: `jarvis-notes list` shows those ids and Nathan can `cat` one.

## N6.1 — Restart and live check with Nathan

- Restart `jarvis-chat`. Ask Nathan on Discord two questions that the seed notes answer (for example dashboard port, and what happens if chat sets `num_ctx`). Then `sudo reboot` is not required: `systemctl restart jarvis-chat` is enough. Confirm `index.md` still has the same ids.
- Done when: both answers use the notes (not "I don't know"), and the count of notes is unchanged after the restart. Record the result in `docs/notes.md`.

## N6.2 — How-to

- File: new `docs/notes.md`.
- Where notes live, format, `jarvis-notes` commands, Discord `!note` / `remember:`, how retrieval is capped, and that D7 is conversation history not notes.
- Done when: the doc is under 80 lines and matches the code.
