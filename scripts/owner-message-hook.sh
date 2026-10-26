#!/usr/bin/env bash
# owner-message-hook.sh — inject owner messages into the agent's context.
#
# The owner sends a message to the agent from the queue site
# (POST /api/owner-message) or `jarvis-jobs message`. It lands in the agent
# inbox ($JARVIS_DATA_DIR/inbox, default ~/.local/share/jarvis/inbox).
#
# This hook (wired in .claude/settings.json) checks the inbox on every loop
# step, like the interrupt in Claude/Cursor:
#   PreToolUse: if a message is waiting, print it as additionalContext so
#     the model sees it before its next tool call — the running process is
#     never stopped or restarted.
#   Stop: if a message is waiting, block the stop (exit 2, message on
#     stderr) so the model gets one more turn with it.
#
# Only the job's own session consumes messages: jarvis-jobs run exports
# JARVIS_JOB_ID into the agent command, and any other claude session in
# this repo leaves the inbox alone.
set -u

input="$(cat)"
event="$(jq -r '.hook_event_name // empty' <<<"$input")"
stop_active="$(jq -r '.stop_hook_active // false' <<<"$input")"

# Only the session jarvis-jobs started drains the inbox.
[[ -n "${JARVIS_JOB_ID:-}" ]] || exit 0

inbox="${JARVIS_DATA_DIR:-$HOME/.local/share/jarvis}/inbox"
[[ -d "$inbox" ]] || exit 0

# Collect pending messages, oldest first (names start with a UnixNano
# timestamp), capped so one flood cannot blow the context.
max=8192
combined=""
while IFS= read -r f; do
  text="$(cat "$f" 2>/dev/null)" || continue
  rm -f "$f"
  if [[ -n "$combined" ]]; then
    combined+=$'\n\n'
  fi
  combined+="$text"
  (( ${#combined} >= max )) && break
done < <(find "$inbox" -maxdepth 1 -type f -name '*.txt' 2>/dev/null | sort)
[[ -n "$combined" ]] || exit 0

case "$event" in
  Stop)
    # A blocked stop already fed a message to the model; don't loop.
    [[ "$stop_active" == "true" ]] && exit 0
    echo "The owner just sent you a message from the queue site (it takes priority): $combined" >&2
    exit 2
    ;;
  PreToolUse)
    jq -n --arg ctx "The owner sent you a message from the queue site (it takes priority): $combined" \
      '{hookSpecificOutput: {hookEventName: "PreToolUse", additionalContext: $ctx}}'
    ;;
  *)
    ;;
esac
exit 0
