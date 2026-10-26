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
