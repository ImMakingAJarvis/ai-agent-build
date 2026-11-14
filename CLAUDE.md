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

Do NOT commit this note (or any `notes/**` file) into a job's feature PR —
the shared note files are the usual cause of PR merge conflicts. Write it
to the working tree (that satisfies the hook and the vector graph) and
persist it via the small notes-only commit described in "Keep notes" in the
agent's own CLAUDE.md.

## Kanban: read the task's board entry before building

Before starting a job whose title references a kanban entry (a kanban ID
in the title — an 8-char hex task ID like `4546f229`, or a full UUID),
check the board:

    jarvis-kanban show <id>

That prints the task JSON, its epic, its subtasks, and all of its
comments in one go (it accepts a full UUID or a unique prefix). If
`jarvis-kanban` is not installed, read the files directly:
`~/.local/share/jarvis/kanban/tasks/<id>*.json` (the task — `title`,
`content` (detailed instructions / acceptance criteria), `status`,
`priority`, `branch`, `model`), its epic at
`~/.local/share/jarvis/kanban/epics/<epic_id>.json` (theme, description,
sibling tasks), and its comments at
`~/.local/share/jarvis/kanban/comments/` (files whose `task_id` matches).

The board is a file store; the dashboard also serves it at
`http://localhost:8080/api/kanban/tasks` and `/api/kanban/epics`. If no
task file matches the ID in the title, the job stands on its own —
build from the job's instructions.

### Comments

Every kanban task has a local comment thread — JSON files under
`~/.local/share/jarvis/kanban/comments/`, served by the dashboard
(`POST /api/kanban/comments`, `GET /api/kanban/comments?task_id=...`,
`DELETE /api/kanban/comments/{id}`; the task detail page shows and adds
comments). The worker can drive it with `jarvis-kanban`:

    jarvis-kanban show <id>                  # thread with the task
    jarvis-kanban comment <id> "question"    # post a comment (author: agent)

When the worker has a question while building, it may add a comment to
the task — post the question as a comment rather than blocking the round
on it; Nathan reads the thread on the board (it is also searchable in
the vector graph). Comments are ordered oldest first and are deleted
with their task.

Work that lives on Plane or between agents has its own channels:

- **Plane items** carry a comment thread — post with
  `jarvis-board comment <REF> "<what changed, why>"` (model-prefixed;
  see [Work board (Plane)](#work-board-plane)). Cross-round progress on a
  board item goes there.
- **Agent mailboxes** are the agent-to-agent message channel:
  `jarvis-mq ask / tell / answer <identity>` (a question with an optional
  wake, a reply, a handoff — `docs/event-driven.md`, [Mailboxes](docs/event-driven.md#mailboxes-agent-to-agent)).
  A mailbox message tied to a board item (`--board <REF>`) also mirrors
  into that item's Plane comments, and a reply mirrors back to the same
  item.

## Work board (Plane)

The work board is Plane at `http://111.1.2.222:8088` (workspace `jarvis`).
**Board standard (2026-10-31, Nathan's call): one project per epic.**
Each epic is its own Plane project with a short identifier prefix
(currently E2E, TST, NBR, MOE, QDB, VCH, T2I, WBD — see
`docs/work-board/NOTES.md`); the epic is the project's epic work item
(label `epic`) and its tasks live in the same project, parented to it.
The project is named after the epic title, and `plane-epic` resolves an
existing project by name (case-insensitive) before creating, so a re-run
cannot create a second project for the same epic.
`AAB` (ai_agent_build) is the legacy single project — no new work goes
there.

- New epic = `plane-epic create "<title>" --identifier XXX
  [--description]`. It scaffolds the project (standard states, epic
  label, workspace members as editors), creates the epic item, adds the
  identifier to `PLANE_PROJECTS` in `~/.config/jarvis/agent.env` and
  restarts `plane-bridge`. That is the only supported way to add a
  project — do not create projects by hand or API.
- Item refs use the prefix: `T2I-3` selects its own project
  (case-insensitive), so `jarvis-board comment T2I-3 "..."` needs no
  `--project`. UUIDs and bare sequences fall back to `--project` →
  `$PLANE_PROJECT` → `AAB`.
- When a job maps to a Plane item (a prefixed id in the job title, or
  `board`/`item` fields in the job's state), post a model-prefixed
  comment whenever the agent touches that item:

      jarvis-board comment --model <model> T2I-3 "<what changed, why>"

  The model is `$JARVIS_MODEL` from the round env (the CLI defaults to
  "jarvis-brain"). Comment convention: `<model>: <what and why>`.
- Discord status messages carry the board link:

      jarvis-discord notify --item T2I-3 "<status>"
- Other subcommands: `jarvis-board item <ref>` (one-line summary with
  state), `jarvis-board link <ref>` (URL only), `jarvis-board state <ref>
  <State> [msg...]` and `jarvis-board done <ref> [msg...]` (move the item;
  the message, when given, is also posted as a model-prefixed comment).
  Flags go BEFORE the positional args.
- Link format: `<PLANE_URL>/jarvis/browse/<PREFIX-<n>>` (browse works
  for any project in the workspace).
- `plane-bridge` watches every project in `PLANE_PROJECTS` (workspace
  webhooks, gated per project).
- **When the worker is done with a task, it updates the Plane item.**
  Until C1 writes states back automatically, the worker owns the
  transition: after the PR is open and Discord is notified, move the
  item and leave the outcome as a model-prefixed comment:

      jarvis-board done T2I-3 "done: <what shipped> (PR #n)"

  A failed or stuck job goes to `Blocked` with the reason
  (`jarvis-board state T2I-3 Blocked "..."`) instead of being left
  where it is.
