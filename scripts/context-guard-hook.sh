#!/usr/bin/env bash
# context-guard-hook.sh — PreToolUse hook (every tool): checks the round's
# context window before each turn and keeps it from overflowing the
# engine's hard --ctx-size.
#
# Why: the local engines have a fixed context (brain 128K, iGPU 32K). The
# harness estimates tokens with its own tokenizer; the engine counts with
# the model's. When the estimate runs hot, one long turn can push the real
# prompt past --ctx-size and the API call dies mid-round with a context
# overflow — the round is lost with its work half done. The harness
# auto-compacts near the top of the window, but that leaves no room for the
# mismatch. This hook measures the actual prompt size (the last API usage
# in the session transcript) before every tool call and then:
#
#   always:  writes ~/.local/state/jarvis/context-window.json (read by
#            jarvis-stack-status) and a throttled context-window.log line;
#   >= WARN (65%): one non-blocking reminder per 5% band — keep reads
#            small, pipe long output through head/tail;
#   >= TRIM (75%): one reminder per band — write progress to notes so a
#            fresh conversation can resume, stop growing the context;
#   >= HARD (85%): blocks the context-heavy tools (Read, non-whitelisted
#            Bash, WebFetch, WebSearch, Agent, NotebookEdit) with the
#            compress-or-exit instruction. The whitelisted escape hatches
#            (jarvis-jobs yield/finish, jarvis-discord, notes, small
#            utilities) still pass, and Write/Edit are never blocked.
#
# Exit 0 = allow (optionally with additionalContext). Exit 2 = block;
# stderr goes back to the model. Anything unexpected fails open: this hook
# must never be the reason a round cannot run.
#
# Thresholds are percentages of CLAUDE_CODE_MAX_CONTEXT_TOKENS and may be
# overridden per round with CTX_GUARD_WARN / CTX_GUARD_TRIM / CTX_GUARD_HARD.
# CTX_GUARD_STATE_DIR moves the state files (tests).
set -u

input="$(cat)"

# Only fixed-window job rounds are guarded: the window env is set by
# agent-local (local + igpu drivers); sonnet/opus/interactive sessions have
# a window the API manages itself.
[[ -n "${JARVIS_JOB_ID:-}" ]] || exit 0
window="${CLAUDE_CODE_MAX_CONTEXT_TOKENS:-}"
[[ "$window" =~ ^[0-9]+$ ]] || exit 0

command -v jq >/dev/null 2>&1 || exit 0
tool="$(jq -r '.tool_name // empty' <<<"$input" 2>/dev/null)" || exit 0
session_id="$(jq -r '.session_id // empty' <<<"$input" 2>/dev/null)" || exit 0
transcript="$(jq -r '.transcript_path // empty' <<<"$input" 2>/dev/null)" || exit 0
[[ -n "$tool" && -n "$session_id" && -f "$transcript" ]] || exit 0

# Prompt size of the NEXT api call = the last usage's input + cached input
# tokens plus the output it just produced (all of it re-sends next turn).
# The newest usage sits within one tool-result line of the file end, so a
# bounded tail is enough; a transcript older than the tail has no usage in
# it yet and passes through.
usage_line="$(tail -c 8388608 "$transcript" 2>/dev/null | tac | grep -m1 '"usage":{' || true)"
[[ -n "$usage_line" ]] || exit 0
num() { grep -o "\"$1\":[0-9]*" <<<"$usage_line" | head -1 | cut -d: -f2; }
in_tok="$(num input_tokens)"
cache_read="$(num cache_read_input_tokens)"
cache_create="$(num cache_creation_input_tokens)"
out_tok="$(num output_tokens)"
[[ "$in_tok" =~ ^[0-9]+$ ]] || exit 0
prompt=$(( in_tok + ${cache_read:-0} + ${cache_create:-0} + ${out_tok:-0} ))

# pct x 10 (integer): 734 = 73.4%
pct10=$(( prompt * 1000 / window ))
pct="$(( pct10 / 10 )).$(( pct10 % 10 ))"

warn="${CTX_GUARD_WARN:-65}"
trim="${CTX_GUARD_TRIM:-75}"
hard="${CTX_GUARD_HARD:-85}"
tier=ok
if   (( pct10 >= hard * 10 )); then tier=hard
elif (( pct10 >= trim * 10 )); then tier=trim
elif (( pct10 >= warn * 10 )); then tier=warn
fi

state_dir="${CTX_GUARD_STATE_DIR:-$HOME/.local/state/jarvis}"
mkdir -p "$state_dir" 2>/dev/null || exit 0
state="$state_dir/context-window.json"
log="$state_dir/context-window.log"

# Previous state drives the band throttle (a reminder fires once per 5%
# band, so a hot round gets at most a handful, not one per tool call) and
# the log throttle. A new session resets the bands.
prev_session=""; prev_tier="ok"; prev_band=-1
last_warn_band=-1; last_trim_band=-1; last_log_ts=""
if [[ -f "$state" ]]; then
  prev_session="$(jq -r '.session_id // empty' "$state" 2>/dev/null)"
  prev_tier="$(jq -r '.tier // "ok"' "$state" 2>/dev/null)"
  prev_band="$(jq -r '.band // -1' "$state" 2>/dev/null)"
  last_warn_band="$(jq -r '.last_warn_band // -1' "$state" 2>/dev/null)"
  last_trim_band="$(jq -r '.last_trim_band // -1' "$state" 2>/dev/null)"
  last_log_ts="$(jq -r '.last_log_ts // empty' "$state" 2>/dev/null)"
fi
band=$(( pct10 / 50 ))
[[ "$prev_session" == "$session_id" ]] || { last_warn_band=-1; last_trim_band=-1; }

remind=""
if   [[ "$tier" == "warn" ]] && (( band > last_warn_band )); then
  last_warn_band=$band
  remind="CONTEXT GUARD: the conversation is at ${pct}% of the ${window}-token engine window. Keep this turn small: pipe long command output through head/tail, read files with offset/limit, and do not re-read files you have already read."
elif [[ "$tier" == "trim" ]] && (( band > last_trim_band )); then
  last_trim_band=$band
  remind="CONTEXT GUARD: ${pct}% of the ${window}-token engine window is used — start compressing. Write your progress to notes/ now (files changed, decisions, exact next steps) so a fresh conversation can resume, and stop growing the context: no new big reads, pipe output through head/tail. If the job can wait on something (a corr, a review, a window), yield it with jarvis-jobs yield."
fi

ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Log at most every 15 s, and always on a band or tier change.
append=0
if [[ "$tier" != "$prev_tier" || $band -gt $prev_band ]]; then
  append=1
else
  last_epoch="$(date -d "$last_log_ts" +%s 2>/dev/null || echo 0)"
  (( $(date +%s) - last_epoch >= 15 )) && append=1
fi
if (( append )); then
  printf '%s job=%s sess=%s model=%s tok=%s pct=%s tier=%s\n' \
    "$ts" "${JARVIS_JOB_ID}" "${session_id:0:8}" "${JARVIS_MODEL:-?}" \
    "$prompt" "$pct" "$tier" >> "$log" 2>/dev/null || true
  [[ -f "$log" ]] && (( $(stat -c%s "$log" 2>/dev/null || echo 0) > 500000 )) && \
    mv -f "$log" "$log.1" 2>/dev/null || true
fi

# Atomic state write; a failed write never blocks the tool call.
tmp="$state.tmp.$$"
jq -n \
  --arg job "$JARVIS_JOB_ID" --arg sess "$session_id" --arg model "${JARVIS_MODEL:-}" \
  --arg tier "$tier" --arg ts "$ts" --arg logts "$ts" \
  --argjson window "$window" --argjson prompt "$prompt" --argjson pct "$pct" \
  --argjson band "$band" --argjson lw "$last_warn_band" --argjson lt "$last_trim_band" \
  '{job_id:$job, session_id:$sess, model:$model, window:$window,
    prompt_tokens:$prompt, pct:$pct, tier:$tier, ts:$ts,
    band:$band, last_warn_band:$lw, last_trim_band:$lt, last_log_ts:$logts}' \
  > "$tmp" 2>/dev/null && mv -f "$tmp" "$state" || rm -f "$tmp"

# ---- the hard tier: block context-heavy tools, keep the escape hatches ----
if [[ "$tier" == "hard" ]]; then
  heavy=0
  case "$tool" in
    Read|WebFetch|WebSearch|Agent|NotebookEdit) heavy=1 ;;
    Bash)
      cmd="$(jq -r '.tool_input.command // empty' <<<"$input" 2>/dev/null)"
      # First real word: skip VAR=val prefixes and sudo.
      word=""
      for x in $cmd; do
        case "$x" in
          *=*) continue ;;
          sudo) continue ;;
          *) word="${x##*/}"; break ;;
        esac
      done
      case "$word" in
        jarvis-*) heavy=0 ;;
        gh|git|echo|ls|cat|head|tail|wc|mv|cp|mkdir|rm|touch|date|whoami|true) heavy=0 ;;
        agent-run-restart|agent-reboot|jarvis-notes) heavy=0 ;;
        *) heavy=1 ;;
      esac
      ;;
  esac
  if (( heavy )); then
    cat >&2 <<MSG
CONTEXT GUARD: this conversation is at ${pct}% of the engine's hard ${window}-token window (${prompt} tokens used). One more big read or command can overflow the context and kill the round mid-work. Compress and continue — pick one:

1. YIELD the job (the work resumes in a fresh conversation when what it waits on lands):
   jarvis-jobs yield ${JARVIS_JOB_ID} --note "..." --in-flight "<what is half done>" --next "<exact next steps>"
   (only when the job can actually wait: a corr, a review, a window)
2. END THE ROUND NOW: write the state to notes/ (files changed, decisions, exact next steps) and stop calling tools — the loop checkpoints and the next round resumes from the notes.
3. If the work is nearly done, take only tiny steps (small reads, whitelisted commands) until the harness auto-compacts; do not open anything new and big.

Allowed now: jarvis-* helpers, git/gh, notes (Write/Edit), and the small utilities. Blocked: Read, WebFetch/WebSearch, subagents, and large shell commands.
MSG
    exit 2
  fi
fi

if [[ -n "$remind" ]]; then
  jq -n --arg ctx "$remind" \
    '{hookSpecificOutput:{hookEventName:"PreToolUse",additionalContext:$ctx}}'
fi
exit 0
