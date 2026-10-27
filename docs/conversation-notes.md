# Conversation notes

Nathan asked (job `chat-1792753040-89c8`) that the agent take notes from
all of our conversations and keep them somewhere on the machine where the
agent can access them. There are two conversation channels, so there are
two note streams, both under `notes/conversations/` in this repo.

## Streams

1. **Agent rounds** — `notes/conversations/agent-YYYY-MM-DD.md` (in git).
   The coding agent (Claude Code / `agent-run`) appends one `## HH:MM <topic>`
   section per run: what was asked, what was decided and built, what is left
   open. The standing instruction lives in `CLAUDE.md`, which Claude Code
   loads at the start of every session in this repo.
2. **Discord / voice** — `notes/conversations/discord-YYYY-MM-DD.md`
   (gitignored; verbatim and chatty). `jarvis-chat` appends every exchange
   (`[timestamp] Nathan: …` / `[timestamp] Jarvis: …`) right after it
   updates its in-memory history. The in-memory history is only the last
   10 turns, so without this log Discord conversations were lost on every
   service restart.

The agent reads the Discord logs when it needs context from earlier talks
and writes distilled takeaways into its own agent notes — it does not
copy the verbatim log into git.

## The Stop hook (enforcement)

Instructions alone get forgotten, so there is a guard:
`.claude/settings.json` wires a **Stop** hook to
`scripts/notes-stop-hook.sh`. When a Claude Code session tries to end:

- If a note file (`agent-*.md`) was modified after the session started
  (the transcript's first timestamp), the stop is allowed.
- Otherwise the hook exits 2 on stderr with a reminder. Claude Code feeds
  that back to the model, which gets one more turn to write the notes.
- The second stop is always allowed (`stop_hook_active` + a per-session
  `/tmp` marker), so it can never loop.

Test the hook by hand:

```
echo '{"session_id":"test","transcript_path":"'"$(ls -t ~/.claude/projects/-home-agent-ai-agent-build/*.jsonl | head -1)"'","stop_hook_active":false}' \
  | bash scripts/notes-stop-hook.sh; echo "exit=$?"
# first run: exit=2 (blocked once); second run: exit=0
```

## Configuration

- `JARVIS_NOTES_DIR` — where `jarvis-chat` writes the Discord log.
  Unset: `<home>/ai-agent-build/notes/conversations`. Explicitly empty:
  logging disabled (tests use this).
- To stop the agent from being nagged: delete the `hooks` block from
  `.claude/settings.json`. To stop the Discord log: set
  `JARVIS_NOTES_DIR=` (empty) in `/home/agent/.config/jarvis/agent.env`.

## Possible next steps

- A nightly job that summarizes `discord-*.md` into a daily digest in the
  agent notes (so even chatty days leave a distilled record).
- Search over the notes (a small `grep`-based helper or dashboard tab).
