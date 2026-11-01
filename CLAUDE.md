# CLAUDE.md

## Conversation notes (standing instruction from Nathan)

Nathan wants notes from all of our conversations, saved on this machine.

Before ending any run (loop round, PR, or anything else), append a short
section to `notes/conversations/agent-<YYYY-MM-DD>.md` (today's date,
create it if missing):

```
## HH:MM <short topic>
- Asked: <what Nathan wanted>
- Did: <decisions, what was built/changed, PR number if any>
- Open: <anything left for a later round>   (omit if none)
```

Rules:
- Append; never rewrite or delete earlier sections. One `##` per session.
- Keep it under ~30 lines. If the round had nothing worth noting,
  one line saying so is enough.
- Never write secrets (tokens, keys, .env contents) into notes.
- Discord/voice talks are logged verbatim by `jarvis-chat` in
  `notes/conversations/discord-<date>.md` — read them when you need
  context from earlier conversations; do not copy them into agent notes.

A Stop hook (`scripts/notes-stop-hook.sh`, wired in `.claude/settings.json`)
blocks the first stop of a session when no note was written yet. If you
get that reminder, write the section and end the run again.

## Kanban: read the task's board entry before building

Before starting a job whose title references a kanban entry (a kanban ID
in the title — an 8-char hex task ID like `4546f229`, or a full UUID),
check the board:

    ls ~/.local/share/jarvis/kanban/tasks/<id>*.json

If the entry exists, read it all before building:
- the task JSON — `title`, `content` (detailed instructions /
  acceptance criteria), `status`, `priority`, `branch`, `model`
- its epic — `epic_id` →
  `~/.local/share/jarvis/kanban/epics/<epic_id>.json` (theme,
  description, sibling tasks)
- any comments on the task (see below)

The board is a file store; the dashboard also serves it at
`http://localhost:8080/api/kanban/tasks` and `/api/kanban/epics`. If no
task file matches the ID in the title, the job stands on its own —
build from the job's instructions.

### Comments (not implemented yet)

As of 2026-10-27 the kanban has no comment system: task JSONs carry no
comments, and the store/API have no comment endpoints. Adding it is
queued as job `kanban-comments` (Nathan's ask, job
chat-1793163063-9121). When it ships, update this section: list the
task's comments in the read step above, and add that the worker may add
comments to a kanban task when it has questions while building — post
the question as a comment rather than blocking the round on it.

## Work board (Plane)

The work board is Plane at `http://111.1.2.222:8088` (workspace `jarvis`,
project `AAB`, items are `AAB-<n>`). Since P6 the agent identifies itself
on the board:

- When a job maps to a Plane item (an `AAB-<n>` in the job title, or
  `board`/`item` fields in the job's state), post a model-prefixed
  comment whenever the agent touches that item:

      jarvis-board comment --model <model> AAB-<n> "<what changed, why>"

  The model is `$JARVIS_MODEL` from the round env (the CLI defaults to
  "jarvis-brain"). Comment convention: `<model>: <what and why>`.
- Discord status messages carry the board link:

      jarvis-discord notify --item AAB-<n> "<status>"
- Other subcommands: `jarvis-board item AAB-<n>` (one-line summary with
  state), `jarvis-board link AAB-<n>` (URL only). Items accept a UUID,
  `AAB-<n>`, or bare `<n>`. Flags go BEFORE the positional args.
- Link format: `<PLANE_URL>/jarvis/browse/AAB-<n>`.
- Do not move item states from ad-hoc rounds — state writeback is
  owned by the C1 phase of the plan.
