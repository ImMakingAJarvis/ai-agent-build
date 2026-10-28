# Agent notes (vector-graph-backed)

Nathan asked (job `chat-1792998605-81b4`) to use the new vector graph to start
keeping notes on things the agent has done, things about the system, and things
Nathan has asked about.

## What the vector graph already held

The Jarvis Memory Service (`jarvis-memory`, `127.0.0.1:8101`) re-scans every
15 minutes and already indexes: every past job's spec, state and transcript
(`job`/`jobs`), the pending queue, the kanban board, and a snapshot of all repo
docs (`repo-doc` — every `.md`/`.txt`, including `notes/conversations/`). So
"what did we do in job X" was already searchable. What was missing was a home
for the agent's own **categorical, durable** notes and a cheap way to make a
fresh note searchable *immediately*.

## The three buckets

The agent keeps three notes files, one per bucket, in `notes/agent/`:

- **`done.md`** — a curated "shipped" log: one line per finished job/PR.
- **`system.md`** — durable facts about this machine and stack (services,
  ports, endpoints, gotchas). Facts are updated in place when they change.
- **`asked.md`** — the themes of things Nathan has asked about or decided,
  grouped so recurring interests are easy to spot.

These complement the per-session chronological conversation notes in
`notes/conversations/agent-*.md`: those are "what happened this run", these are
"the standing record of what I've learned and what Nathan cares about".

## The tool: `jarvis-notes`

`scripts/jarvis-notes` (installed to `/usr/local/bin/jarvis-notes`, stdlib-only
Python) makes adding a note one cheap command:

```
jarvis-notes add system "engine image must match host glibc 2.43"
jarvis-notes add done "Containerized the stack (PR #83)" --job chat-...
jarvis-notes list system --tail 5
jarvis-notes sync        # push all files now
jarvis-notes stats       # memory-service /stats
```

Each `add`:

1. Appends a UTC-timestamped line to `notes/agent/<bucket>.md`.
2. Pushes the whole file into the memory service via `POST /index`.

The push reuses exactly what the periodic reindexer (`index_repo_docs` in
`scripts/memory_service.py`) would produce — `source=repo-doc`, the file's
repo-relative path, the filename as title — so the `(source, path)` doc is the
same one the reindex maintains. There is no duplicate; the next reindex simply
finds an unchanged hash and does nothing. The effect of the push is that a note
added mid-session is findable by a search *right away*, not up to 15 minutes
later.

If the memory service is down the note is still written to disk and the CLI
warns (`NOT indexed ...`); the next reindex picks it up. The note file is the
source of truth; the graph index is a cache.

## Standing practice

The agent's personal CLAUDE.md (`docs/agent-claude.md`, installed to
`~/.claude/CLAUDE.md`) now tells it to: search the graph before grepping the
filesystem for "what did we do about X", and — at the end of each job — record
the result in the right bucket(s) with `jarvis-notes add`.

## Why not just rely on the 15-minute reindex?

It works, and these files *are* auto-indexed by it. The CLI adds two things:
instant searchability for a note you just wrote (the tight loop the job asked
for), and a single low-friction entry point that enforces the three-bucket
structure and the timestamp format.
