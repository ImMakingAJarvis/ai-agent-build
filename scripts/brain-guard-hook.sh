#!/usr/bin/env bash
# brain-guard-hook.sh — PreToolUse hook: a round running ON the local brain
# may not stop, restart or kill that brain.
#
# Why: the rule has been prose in the round brief since 2026-11-05 and was
# still broken (docs/worker-moe-incident-2026-11-04.md: a local round ran
# `docker compose stop engine` and 11 rounds died; 2026-11-07: a local round
# ran `docker restart jarvis-engine` because the job text said to). A model
# that must remember a rule forgets it; this hook refuses the command.
#
# Scope — it blocks only when ALL of these hold:
#   - the session is a queue round (JARVIS_JOB_ID is set by jarvis-jobs run),
#   - the round is served by the brain engine (ANTHROPIC_BASE_URL points at
#     port 11434, or JARVIS_MODEL is jarvis-brain),
#   - the tool is Bash and the command, at a command position, stops the
#     brain engine (never the iGPU helper `engine-igpu`).
# Sonnet/Opus/iGPU rounds, and any session outside the job loop, pass
# through untouched — they are the drivers that legitimately free the 4090.
#
# Exit 0 = allow. Exit 2 = block; stderr goes back to the model.
# Anything unexpected (no jq, bad payload) fails open: this hook must never
# be the reason a round cannot run a command.
set -u

input="$(cat)"

[[ -n "${JARVIS_JOB_ID:-}" ]] || exit 0

on_brain=0
case "${ANTHROPIC_BASE_URL:-}" in
  *:11434|*:11434/*) on_brain=1 ;;
esac
[[ "${JARVIS_MODEL:-}" == "jarvis-brain" ]] && on_brain=1
(( on_brain )) || exit 0

command -v jq >/dev/null 2>&1 || exit 0
tool="$(jq -r '.tool_name // empty' <<<"$input" 2>/dev/null)" || exit 0
[[ "$tool" == "Bash" ]] || exit 0
cmd="$(jq -r '.tool_input.command // empty' <<<"$input" 2>/dev/null)" || exit 0
[[ -n "$cmd" ]] || exit 0

# The iGPU helper is a different engine and may be restarted freely: drop its
# names before matching so `docker restart jarvis-engine-igpu` passes.
probe="${cmd//jarvis-engine-igpu/IGPU}"
probe="${probe//engine-igpu/IGPU}"

# A command position: start of a line, or after ; & | ( — optionally behind
# sudo. Text inside a grep pattern, an echo or a commit message is not a
# command and does not match (the 2026-11-19 lesson: matching on text that
# merely mentions a thing caused a stop loop).
pos='(^|[;&|(])[[:space:]]*(sudo[[:space:]]+(-n[[:space:]]+)?)?'
eng='(jarvis-)?engine([[:space:]]|$|[;&|)])'

patterns=(
  "${pos}docker[[:space:]]+(stop|restart|kill|rm|pause)[[:space:]]+([^;&|]*[[:space:]])?${eng}"
  "${pos}docker[[:space:]]+compose[[:space:]]+([^;&|]*[[:space:]])?(stop|restart|kill|rm|pause)[[:space:]]+([^;&|]*[[:space:]])?${eng}"
  "${pos}docker[[:space:]]+compose[[:space:]]+([^;&|]*[[:space:]])?down([[:space:]]|$|[;&|)])"
  "${pos}systemctl[[:space:]]+([^;&|]*[[:space:]])?(stop|restart|kill)[[:space:]]+([^;&|]*[[:space:]])?jarvis-engine(\.service)?([[:space:]]|$|[;&|)])"
  "${pos}(pkill|killall)[[:space:]]+([^;&|]*[[:space:]])?[^;&|[:space:]]*llama[^;&|[:space:]]*"
  "${pos}jarvis-swap[[:space:]]+out([[:space:]]|$|[;&|)])"
)

hit=""
for p in "${patterns[@]}"; do
  if grep -Eq -- "$p" <<<"$probe"; then
    hit=1
    break
  fi
done
[[ -n "$hit" ]] || exit 0

cat >&2 <<'MSG'
BLOCKED: this command would stop the brain engine (port 11434) — the model
this round is running on. Your next API call would die with "Connection
refused" and the engine would stay down for every later round
(docs/worker-moe-incident-2026-11-04.md).

Do this instead:
- If the job needs the 4090 free (a VRAM sweep, a benchmark, photo work):
  it belongs on another driver. `jarvis-jobs requeue <id> -driver <the job
  spec's driver, sonnet or igpu>`, leave a Discord note, and end the round.
  Photo work: queue it with the `photo` tag and end the round — the window
  controller swaps the brain out between rounds.
- If the job text itself tells you to restart the brain: do not. Say so on
  Discord and end the round; the owner or an igpu/sonnet round does it.
- The iGPU helper (`jarvis-engine-igpu`) is not the brain; restarting it is
  allowed.
MSG
exit 2
