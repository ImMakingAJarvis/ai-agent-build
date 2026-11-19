#!/usr/bin/env bash
# test_brain_guard_hook.sh — table test for scripts/brain-guard-hook.sh.
# Run: bash scripts/test_brain_guard_hook.sh   (exit 0 = all pass)
set -u
hook="$(cd "$(dirname "$0")" && pwd)/brain-guard-hook.sh"
fail=0

# run <want-exit> <env assignments or "-"> <command text>
run() {
  local want="$1" envs="$2" cmd="$3" payload got
  payload="$(jq -n --arg c "$cmd" '{tool_name:"Bash", tool_input:{command:$c}}')"
  if [[ "$envs" == "-" ]]; then
    got="$(env -u JARVIS_JOB_ID -u ANTHROPIC_BASE_URL -u JARVIS_MODEL bash "$hook" <<<"$payload" >/dev/null 2>&1; echo $?)"
  else
    # shellcheck disable=SC2086
    got="$(env -u JARVIS_JOB_ID -u ANTHROPIC_BASE_URL -u JARVIS_MODEL $envs bash "$hook" <<<"$payload" >/dev/null 2>&1; echo $?)"
  fi
  if [[ "$got" != "$want" ]]; then
    echo "FAIL want=$want got=$got env=[$envs] cmd=[$cmd]"
    fail=1
  fi
}

BRAIN="JARVIS_JOB_ID=j1 ANTHROPIC_BASE_URL=http://localhost:11434"
BRAIN2="JARVIS_JOB_ID=j1 JARVIS_MODEL=jarvis-brain"
IGPU="JARVIS_JOB_ID=j1 ANTHROPIC_BASE_URL=http://localhost:11435 JARVIS_MODEL=jarvis-igpu"
SONNET="JARVIS_JOB_ID=j1 JARVIS_MODEL=sonnet"

# Blocked on a brain round.
run 2 "$BRAIN"  'docker compose stop engine'
run 2 "$BRAIN"  'docker compose restart engine'
run 2 "$BRAIN"  'docker stop jarvis-engine'
run 2 "$BRAIN"  'docker restart jarvis-engine'
run 2 "$BRAIN"  'cd /home/agent/ai-agent-build && docker compose stop engine && nvidia-smi'
run 2 "$BRAIN"  'sudo -n docker stop -t 30 jarvis-engine'
run 2 "$BRAIN"  'docker compose -f docker-compose.yml stop engine'
run 2 "$BRAIN"  'docker compose down'
run 2 "$BRAIN"  'sudo systemctl stop jarvis-engine'
run 2 "$BRAIN"  'sudo systemctl restart jarvis-engine.service'
run 2 "$BRAIN"  'pkill -f llama-server'
run 2 "$BRAIN"  'pkill -f "llama-server|llama-bench"'
run 2 "$BRAIN"  'killall llama-server'
run 2 "$BRAIN"  'jarvis-swap out --for image.generate'
run 2 "$BRAIN"  $'echo start\ndocker stop jarvis-engine'
run 2 "$BRAIN2" 'docker stop jarvis-engine'

# Allowed on a brain round: the iGPU helper, read-only use, other services,
# and text that only mentions the command.
run 0 "$BRAIN"  'docker restart jarvis-engine-igpu'
run 0 "$BRAIN"  'docker compose restart engine-igpu'
run 0 "$BRAIN"  'docker compose restart dashboard chat'
run 0 "$BRAIN"  'docker compose up -d engine'
run 0 "$BRAIN"  'docker logs --tail 50 jarvis-engine'
run 0 "$BRAIN"  'docker inspect jarvis-engine'
run 0 "$BRAIN"  'jarvis-swap status'
run 0 "$BRAIN"  'jarvis-swap handoff'
run 0 "$BRAIN"  'grep -rn "docker compose stop engine" docs/'
run 0 "$BRAIN"  'git commit -m "docs: never run docker stop jarvis-engine from a local round"'
run 0 "$BRAIN"  'echo "pkill llama-server is forbidden"'
run 0 "$BRAIN"  'pgrep -af llama-server'
run 0 "$BRAIN"  'go build ./... && go test ./...'

# Other drivers and non-job sessions pass through.
run 0 "$IGPU"   'docker compose stop engine'
run 0 "$SONNET" 'docker compose stop engine'
run 0 "-"       'docker compose stop engine'

# Non-Bash tools and bad payloads fail open.
got="$(JARVIS_JOB_ID=j1 JARVIS_MODEL=jarvis-brain bash "$hook" <<<'{"tool_name":"Read","tool_input":{"file_path":"docker stop jarvis-engine"}}' >/dev/null 2>&1; echo $?)"
[[ "$got" == 0 ]] || { echo "FAIL non-Bash tool blocked ($got)"; fail=1; }
got="$(JARVIS_JOB_ID=j1 JARVIS_MODEL=jarvis-brain bash "$hook" <<<'not json' >/dev/null 2>&1; echo $?)"
[[ "$got" == 0 ]] || { echo "FAIL bad payload blocked ($got)"; fail=1; }

if (( fail )); then
  echo "brain-guard-hook: FAILED"
  exit 1
fi
echo "brain-guard-hook: all cases pass"
