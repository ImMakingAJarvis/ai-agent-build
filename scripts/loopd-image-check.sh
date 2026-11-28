#!/usr/bin/env bash
# loopd-image-check — what a round needs, checked against the loopd image
# (card ELR-100, M4.1h). It runs inside the container; the image carries
# no repo and no host tools, so copy the script in first:
#
#   docker cp scripts/loopd-image-check.sh jarvis-loopd:/tmp/
#   docker exec jarvis-loopd bash /tmp/loopd-image-check.sh
#
# It prints one line per capability, `ok` or `MISSING`, and exits 1 when
# any capability is MISSING. The capabilities are: a git push dry run
# through the app-token credential helper (the same helper the host's
# ~/.gitconfig wires), gh, the Go toolchain, and each jarvis-* CLI a
# round calls.
set -u

# Each jarvis-* CLI a round calls (the round brief, CLAUDE.md).
JARVIS_CLIS="jarvis-jobs jarvis-mq jarvis-board jarvis-discord jarvis-notes
jarvis-secrets jarvis-kanban jarvis-stack-status jarvis-helper jarvis-swap
jarvis-review"

# The credential helper a round pushes through: ghapp-token supplies the
# password. It is passed through git's env config so the check carries
# the helper itself — the container has no copy of the host's
# ~/.gitconfig.
readonly GIT_PUSH_HELPER='!f() { [ "$1" = get ] && { echo username=x-access-token; echo "password=$(ghapp-token)"; }; }; f'

missing=0

report() { # <capability> <rc>
  if [ "$2" -eq 0 ]; then
    printf '%-30s ok\n' "$1"
  else
    printf '%-30s MISSING\n' "$1"
    missing=1
  fi
}

have() { command -v "$1" >/dev/null 2>&1; }

# git-push: a dry-run push of HEAD to origin through the app-token
# helper — it verifies the auth without pushing. Needs git,
# ghapp-token, and a repo with an origin remote: the repo is
# $LOOPD_CHECK_REPO (default: the cwd, a round's worktree).
check_git_push() {
  have git || return 1
  have ghapp-token || return 1
  local repo="${LOOPD_CHECK_REPO:-.}"
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper \
    GIT_CONFIG_VALUE_0="$GIT_PUSH_HELPER" \
    git -C "$repo" push --dry-run --quiet origin HEAD >/dev/null 2>&1
}

# gh: present and runnable (rounds drive it as GH_TOKEN=$(ghapp-token) gh).
check_gh() { have gh && gh --version >/dev/null 2>&1; }

# go: the toolchain is present and runnable (the round builds and tests
# the repo with it; a scratch build is not the check's job).
check_go() { have go && go version >/dev/null 2>&1; }

check_git_push; report "git push (dry run, app token)" $?
check_gh;       report "gh" $?
check_go;       report "go (build/test)" $?
for cli in $JARVIS_CLIS; do
  have "$cli"; report "$cli" $?
done
exit "$missing"
