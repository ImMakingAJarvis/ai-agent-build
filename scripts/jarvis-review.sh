#!/usr/bin/env bash
# jarvis-review — the sender side of the reviewer flow (plan §3.8, EVB-36):
# the brain finishes a PR and hands it to another model (sonnet / planner)
# for review, then parks until the review lands.
#
# One command does the whole handoff:
#   1. posts a `review_request` (PR link + summary) to the reviewer's inbox,
#      with a WAKE attached: the sender's parked job wakes when the
#      reviewer's `answer` (same corr) lands on the sender's inbox — the
#      listener-wake pair from task EVB-35;
#   2. enqueues the reviewer job (driver sonnet/opus, class fix, high
#      priority so the loop pulls it right after the sender yields);
#   3. mirrors the request onto the board item (--board) so the exchange is
#      readable on Plane. The reviewer's answer mirrors too (it carries the
#      same board ref).
#
# Reviewer pick rule (the documented simple one — pass --reviewer to
# override):
#   sonnet   — code PRs: a `claude` session that can read the diff and run
#              the go gate in a throwaway worktree;
#   planner  — docs/plan-only PRs: no code to test, so the stronger
#              planning review (opus driver) is the better fit.
# The default is sonnet.
#
# Usage:
#   jarvis-review request <PR-N> [--reviewer sonnet|planner] [--board REF]
#                          [--branch BR] [--wake JOB] [--timeout D]
#                          [--summary TEXT]
#
#   <PR-N>      the PR number to review (e.g. 253)
#   --reviewer  sonnet (default) or planner
#   --board REF the Plane item (e.g. EVB-38) the exchange is mirrored on
#   --branch BR the PR's branch (default: detected via `gh pr view`)
#   --wake JOB  the sender's parked agentjob to wake on the answer
#               (default: $JARVIS_JOB_ID — the round that runs this)
#   --timeout D listener give-up (default 2h)
#   --summary T one-line summary of what the PR does (goes in the subject)
#
# After this returns, the sender yields and ends the round:
#   jarvis-jobs yield $JARVIS_JOB_ID --wait-on <corr> \
#     --note "..." --in-flight "PR #N open, review requested" \
#     --next "read the review (jarvis-mq inbox brain), address comments,
#             gh-pr-wait <N>, notify"
#
# The reply comes back on the sender's inbox; the woken round reads it with
# `jarvis-mq inbox brain` (then `take`), addresses the findings, and
# finishes the normal PR workflow.
set -euo pipefail

usage() {
  sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

cmd="${1:-}"; [[ $# -gt 0 ]] && shift
[[ "$cmd" == "request" ]] || { usage 2; }

pr="" reviewer="sonnet" board="" branch="" wake="${JARVIS_JOB_ID:-}" \
  timeout="2h" summary=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --reviewer) reviewer="${2:?}"; shift 2 ;;
    --board)    board="${2:?}"; shift 2 ;;
    --branch)   branch="${2:?}"; shift 2 ;;
    --wake)     wake="${2:?}"; shift 2 ;;
    --timeout)  timeout="${2:?}"; shift 2 ;;
    --summary)  summary="${2:?}"; shift 2 ;;
    -h|--help)  usage 0 ;;
    -*)         echo "jarvis-review: unknown flag $1" >&2; usage 2 ;;
    *)          [[ -z "$pr" ]] || { echo "jarvis-review: unexpected arg $1" >&2; exit 2; }
                pr="$1"; shift ;;
  esac
done

[[ "$pr" =~ ^[0-9]+$ ]] || { echo "jarvis-review: <PR-N> must be a number (got $pr)" >&2; exit 2; }
case "$reviewer" in
  sonnet)  rdriver="sonnet" ridentity="sonnet" ;;
  planner) rdriver="opus"   ridentity="planner" ;;
  *) echo "jarvis-review: --reviewer must be sonnet or planner (got $reviewer)" >&2; exit 2 ;;
esac
[[ -n "$wake" ]] || { echo "jarvis-review: no --wake and \$JARVIS_JOB_ID is unset — pass --wake <job>" >&2; exit 2; }

# The PR's branch, for the reviewer's throwaway worktree (default: detect).
if [[ -z "$branch" ]]; then
  branch="$(GH_TOKEN="$(ghapp-token)" gh pr view "$pr" --repo ImMakingAJarvis/ai-agent-build --json headRefName --jq .headRefName 2>/dev/null || true)"
fi
[[ -n "$branch" ]] || branch="phase-event-driven-brain"

repo_url="https://github.com/ImMakingAJarvis/ai-agent-build"
pr_url="${repo_url}/pull/${pr}"
corr="rev-${pr}-$(date +%s%N | tail -c 7)"
summary="${summary:-review PR #${pr}}"

echo "jarvis-review: PR #${pr} (${branch}) -> reviewer ${reviewer} (driver ${rdriver}, inbox agent.${ridentity}.inbox)"
echo "jarvis-review: corr=${corr} wake=${wake} board=${board:--}"

# 1. The review_request: to the reviewer's inbox, wake on the answer,
#    mirrored to the board item. The subject is the one-line summary; the
#    body carries the link + context.
body="PR: ${pr_url}
branch: ${branch}
summary: ${summary}
Review it (diff + the go gate in a throwaway worktree), then answer with your verdict. This is corr ${corr} — the answer must carry it (it is the wake key)."

tell_args=(tell "$ridentity" --kind review_request --from brain
  --wake "$wake" --corr "$corr" --timeout "$timeout")
[[ -n "$board" ]] && tell_args+=(--board "$board")
tell_args+=(--body "$body" "${summary}: review request for PR #${pr}")
if ! jarvis-mq "${tell_args[@]}"; then
  echo "jarvis-review: review_request failed to publish" >&2
  exit 1
fi

# 2. The reviewer job — self-contained for a fresh sonnet/opus session. Its
#    round prompt carries the reviewer's inbox (the mailbox drain step), and
#    the instructions below are the whole job.
reviewer_instructions=$(cat <<EOF
You are the PR reviewer (${ridentity}) in the EVB-36 reviewer flow. A
review_request for PR #${pr} is waiting in your inbox
(agent.${ridentity}.inbox). Do the review and answer — that answer is what
wakes the sender's parked job, so the corr MUST be ${corr}.

Steps:
1. Read the request:  jarvis-mq inbox ${ridentity}
   (find the review_request, corr ${corr}: PR #${pr}, branch ${branch},
   summary: ${summary})
2. Review the PR WITHOUT touching the shared working tree
   (/home/agent/ai-agent-build is the brain's live tree):
   GH_TOKEN=\$(ghapp-token) gh pr view ${pr} --json title,body,files,mergeable
   GH_TOKEN=\$(ghapp-token) gh pr diff ${pr}
   git -C /home/agent/ai-agent-build fetch origin pull/${pr}/head
   git -C /home/agent/ai-agent-build worktree add --detach /tmp/review-pr-${pr} FETCH_HEAD
   cd /tmp/review-pr-${pr} && go build ./... && go vet ./... && go test ./...
   Judge: correctness of the diff, new code covered by tests, docs match the
   code, and nothing stray in the diff (notes/**, secrets, .pyc, binaries).
3. Post the verdict as the answer (subject = one-line verdict, body = the
   findings, one per line, file:line where useful):
   jarvis-mq answer brain --from ${ridentity} --corr ${corr} ${board:+--board ${board}} "<approve|request_changes>: <verdict>" --body "<findings>"
   Verdict is "approve" when the PR is mergeable and any findings are
   non-blocking; "request_changes" when something must be fixed first.
4. Clear the request:  jarvis-mq take ${ridentity}
5. Clean up:  git -C /home/agent/ai-agent-build worktree remove --force /tmp/review-pr-${pr}
6. End the round.

Hard rules: read-only on the repo (the worktree is the only place you build).
No pushes, no commits, no PR edits, no engine/stack changes, no notes. If you
cannot complete the review (branch missing, a gate failure you cannot
explain), still answer — with "request_changes: could not complete review —
<why>" — so the sender's job wakes instead of timing out.
EOF
)

job_id="$(jarvis-jobs add -driver "$rdriver" -class fix -priority 100 \
  -id "review-pr-${pr}-$(date +%s)" \
  -title "review PR #${pr} (corr ${corr})" \
  -instructions "$reviewer_instructions")"

echo "jarvis-review: reviewer job ${job_id} enqueued (driver ${rdriver}, class fix, priority 100)"
echo
echo "Next (sender): yield and end the round — the loop pulls ${job_id},"
echo "the reviewer answers on agent.brain.inbox (corr ${corr}), the wake fires."
echo
echo "  jarvis-jobs yield ${wake} --wait-on ${corr} \\"
echo "    --note \"PR #${pr} open, review requested of ${reviewer}\" \\"
echo "    --in-flight \"PR #${pr} (${branch}) open; review_request corr=${corr} on agent.${ridentity}.inbox\" \\"
echo "    --next \"read the review: jarvis-mq inbox brain (then take); address the findings; gh-pr-wait ${pr} exit 0; jarvis-discord notify; finish\""
echo
echo "corr=${corr}"
