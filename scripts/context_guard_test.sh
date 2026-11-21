#!/usr/bin/env bash
# context_guard_test.sh — table test for scripts/context-guard-hook.sh.
# Run: bash scripts/context_guard_test.sh   (exit 0 = all pass)
set -u
hook="$(cd "$(dirname "$0")" && pwd)/context-guard-hook.sh"
fail=0

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STATE="$TMP/state"
TR="$TMP/transcript.jsonl"
SESSION="sess-$(date +%s)"
WINDOW=131072

mk_usage() { # in cache_read cache_create out  (compact, like a real transcript)
  jq -cn --argjson i "$1" --argjson r "$2" --argjson c "$3" --argjson o "$4" \
    '{type:"assistant", message:{role:"assistant",
       usage:{input_tokens:$i, cache_read_input_tokens:$r,
              cache_creation_input_tokens:$c, output_tokens:$o}}}' > "$TR"
}

call() { # $1=tool  $2=Bash command ("" for non-Bash tools)
  local tool="$1" cmd="${2-}" p
  if [[ -n "$cmd" ]]; then
    p="$(jq -n --arg t "$tool" --arg c "$cmd" --arg s "$SESSION" --arg tr "$TR" \
      '{tool_name:$t, tool_input:{command:$c}, session_id:$s, transcript_path:$tr}')"
  else
    p="$(jq -n --arg t "$tool" --arg s "$SESSION" --arg tr "$TR" \
      '{tool_name:$t, tool_input:{}, session_id:$s, transcript_path:$tr}')"
  fi
  HOOK_OUT="$(JARVIS_JOB_ID=t1 JARVIS_MODEL=jarvis-brain \
    CLAUDE_CODE_MAX_CONTEXT_TOKENS="$WINDOW" CTX_GUARD_STATE_DIR="$STATE" \
    bash "$hook" <<<"$p" 2>"$TMP/err.txt")"; HOOK_RC=$?
  HOOK_ERR="$(cat "$TMP/err.txt")"
}

expect_rc() { # $1=want  $2=desc
  if [[ "$HOOK_RC" != "$1" ]]; then
    echo "FAIL want=$1 got=$HOOK_RC  $2"; fail=1
  fi
}
expect_grep() { # $1=what  $2=where(out|err)  $3=desc
  local hay="$([[ "$2" == err ]] && printf '%s' "$HOOK_ERR" || printf '%s' "$HOOK_OUT")"
  if ! grep -q "$1" <<<"$hay"; then
    echo "FAIL missing [$1] in $2  $3"; fail=1
  fi
}
expect_not_grep() { # $1=what  $2=where  $3=desc
  local hay="$([[ "$2" == err ]] && printf '%s' "$HOOK_ERR" || printf '%s' "$HOOK_OUT")"
  if grep -q "$1" <<<"$hay"; then
    echo "FAIL found [$1] in $2  $3"; fail=1
  fi
}

# --- A. low (32.0%): exit 0, state written, no reminder -------------------
rm -rf "$STATE"
mk_usage 40000 0 1000 1000
call Read ""
expect_rc 0 "ok tier: Read allowed"
expect_not_grep "additionalContext" out "ok tier: no reminder"
[[ -f "$STATE/context-window.json" ]] || { echo "FAIL: state file not written"; fail=1; }
[[ "$(jq -r .tier "$STATE/context-window.json")" == ok ]] \
  || { echo "FAIL: state tier is $(jq -r .tier "$STATE/context-window.json"), want ok"; fail=1; }
[[ "$(jq -r .prompt_tokens "$STATE/context-window.json")" == 42000 ]] \
  || { echo "FAIL: state prompt_tokens $(jq -r .prompt_tokens "$STATE/context-window.json"), want 42000"; fail=1; }
[[ -f "$STATE/context-window.log" ]] || { echo "FAIL: log file not written"; fail=1; }

# --- B. warn: exit 0 + reminder, throttled per 5% band -------------------
mk_usage 89000 0 1000 1000   # 91000 -> 69.4%, band 13
call Read ""
expect_rc 0 "warn tier: Read allowed"
expect_grep "additionalContext" out "warn tier: reminder in stdout"
expect_grep "CONTEXT GUARD" out "warn tier: reminder text"
call Read ""   # same band: throttled
expect_rc 0 "warn tier 2nd call: allowed"
expect_not_grep "additionalContext" out "warn tier 2nd call: no repeat reminder"
mk_usage 90500 0 1000 1000   # 92500 -> 70.6%, band 14
call Read ""
expect_grep "additionalContext" out "warn tier next band: reminder again"

# --- C. trim (79.9%): exit 0 + compress reminder --------------------------
mk_usage 103857 0 1000 1000  # 104857 -> 79.9%
call Read ""
expect_rc 0 "trim tier: Read allowed"
expect_grep "start compressing" out "trim tier: compress reminder"

# --- D. hard (92.1%): heavy tools blocked, escape hatches pass ------------
mk_usage 119832 0 1000 1000  # 120832 -> 92.1%
call Read ""
expect_rc 2 "hard tier: Read blocked"
expect_grep "CONTEXT GUARD" err "hard tier: block message to model"
expect_grep "jarvis-jobs yield" err "hard tier: yield option in message"
[[ "$(jq -r .tier "$STATE/context-window.json")" == hard ]] \
  || { echo "FAIL: state tier not hard"; fail=1; }

call Bash 'jarvis-jobs yield t1 --note "x" --in-flight "y" --next "z"'
expect_rc 0 "hard tier: jarvis-jobs yield allowed"
call Bash 'sudo jarvis-jobs finish t1 0'
expect_rc 0 "hard tier: sudo jarvis-jobs allowed"
call Bash 'FOO=bar jarvis-secrets list'
expect_rc 0 "hard tier: VAR= jarvis-secrets allowed"
call Bash 'git status'
expect_rc 0 "hard tier: git allowed"
call Bash 'head -20 /var/log/x.log'
expect_rc 0 "hard tier: head allowed"
call Bash 'grep -rn docker docs/'
expect_rc 2 "hard tier: grep blocked"
call Bash 'python3 scripts/something.py'
expect_rc 2 "hard tier: python3 blocked"
call Bash 'curl -s http://127.0.0.1:11434/api/tags'
expect_rc 2 "hard tier: curl blocked"

# Write/Edit stay open at any tier — the compress-and-exit path.
call Write ""
expect_rc 0 "hard tier: Write allowed"
call Edit ""
expect_rc 0 "hard tier: Edit allowed"

# --- E. gating: no job id / no window / no usage / bad payload ------------
# env -u: a round's own env carries JARVIS_JOB_ID / the window, and the
# test must not inherit them.
mk_usage 119832 0 1000 1000
HOOK_OUT="$(env -u JARVIS_JOB_ID CLAUDE_CODE_MAX_CONTEXT_TOKENS="$WINDOW" CTX_GUARD_STATE_DIR="$STATE" \
  bash "$hook" <<<'{"tool_name":"Read","tool_input":{},"session_id":"s","transcript_path":"'"$TR"'"}' 2>/dev/null)"; \
  HOOK_RC=$?
expect_rc 0 "no JARVIS_JOB_ID: fail open"
HOOK_OUT="$(env -u CLAUDE_CODE_MAX_CONTEXT_TOKENS JARVIS_JOB_ID=t1 CTX_GUARD_STATE_DIR="$STATE" \
  bash "$hook" <<<'{"tool_name":"Read","tool_input":{},"session_id":"s","transcript_path":"'"$TR"'"}' 2>/dev/null)"; \
  HOOK_RC=$?
expect_rc 0 "no numeric window: fail open"
printf '{"type":"assistant","message":{"role":"assistant","content":"hi"}}\n' > "$TR"
call Read ""
expect_rc 0 "transcript without usage: fail open"
HOOK_OUT="$(JARVIS_JOB_ID=t1 CLAUDE_CODE_MAX_CONTEXT_TOKENS="$WINDOW" CTX_GUARD_STATE_DIR="$STATE" \
  bash "$hook" <<<'not json' 2>/dev/null)"; HOOK_RC=$?
expect_rc 0 "bad payload: fail open"

if (( fail )); then
  echo "context-guard-hook: FAILED"
  exit 1
fi
echo "context-guard-hook: all cases pass"
