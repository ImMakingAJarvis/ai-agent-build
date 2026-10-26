#!/usr/bin/env bash
# notes-stop-hook.sh — Stop hook: make sure the session leaves notes.
#
# Claude Code feeds the hook a JSON payload on stdin:
#   session_id, transcript_path, stop_hook_active, ...
# Exit 0 = let the stop through. Exit 2 = block the stop; the message on
# stderr is fed back to the model, which gets one more turn (it writes
# the notes, then ends again — this hook lets that through).
#
# A session is excused when a note file was already modified after the
# session started (transcript's first timestamp), i.e. notes were left.
set -u

input="$(cat)"
session_id="$(jq -r '.session_id // empty' <<<"$input")"
transcript="$(jq -r '.transcript_path // empty' <<<"$input")"
stop_active="$(jq -r '.stop_hook_active // false' <<<"$input")"

# No session info, or a previous Stop hook already intervened: don't loop.
[[ -n "$session_id" ]] || exit 0
[[ "$stop_active" == "true" ]] && exit 0

# Notes already left this session?
if [[ -n "$transcript" && -f "$transcript" ]]; then
  start_ts="$(head -1 "$transcript" 2>/dev/null | jq -r '.timestamp // empty' 2>/dev/null)"
  if [[ -n "$start_ts" ]] && \
     find notes/conversations -maxdepth 1 -name 'agent-*.md' -newermt "$start_ts" -print -quit 2>/dev/null | grep -q .; then
    exit 0
  fi
fi

# First stop for this session: block exactly once and say what to do.
marker="/tmp/jarvis-notes-${session_id}"
[[ -f "$marker" ]] && exit 0
touch "$marker"

echo "Before you end this run, leave conversation notes (standing instruction from Nathan): append a short '## HH:MM <topic>' section to notes/conversations/agent-$(date +%F).md — what was asked, what you decided and built, and anything left open. Keep it under ~30 lines, no secrets; if nothing was worth noting, write one line saying so. Then end the run again." >&2
exit 2
