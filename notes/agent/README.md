# Agent notes

The agent's own durable notes, kept in three buckets — one file each:

| File | Bucket | Holds |
|---|---|---|
| `done.md` | done | things the agent has done (a curated "shipped" log, one line per job/PR) |
| `system.md` | system | durable facts about this machine and stack (services, endpoints, gotchas) |
| `asked.md` | asked | things Nathan has asked about, decided, or asked repeatedly |

These are **categorical and cumulative** — a fact stays in one file and is
updated in place when it changes. They complement (not replace) the per-session
chronological conversation notes in `notes/conversations/agent-*.md`.

## Keeping them

```
jarvis-notes add system "HA MCP endpoint is /api/mcp; /api/mcp_server/sse 404s"
jarvis-notes add done "Fixed the mic buffer bug (PR #69)" --job chat-179...
jarvis-notes add asked "Nathan keeps asking about GPU power cost"
jarvis-notes list [done|system|asked] [--tail N]
jarvis-notes sync            # push all files to the vector graph now
```

Every `add` appends a timestamped line **and** pushes the file into the Jarvis
Memory Service (the vector graph) right away, so a fresh note is searchable in
the *same* session instead of waiting for the 15-minute reindex. The push
reuses the `repo-doc` source and the file's repo-relative path, so the periodic
reindexer maintains the exact same doc — no duplicates. If the service is down
the note is still written to disk and picked up on the next reindex.

Because these files are markdown in the repo, the vector graph also indexes them
automatically on every reindex even without the CLI — the CLI just makes adding
cheap and makes a note searchable immediately.

See `docs/agent-notes.md` for the full how-it-works.
