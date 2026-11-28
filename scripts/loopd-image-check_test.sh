#!/usr/bin/env bash
# loopd-image-check_test.sh — the stub-PATH test for
# scripts/loopd-image-check.sh (card ELR-100, M4.1h): with a stub PATH
# where every capability is present the check exits 0 and prints no
# MISSING line; with one tool absent it exits 1 and reports that tool
# MISSING.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/loopd-image-check.sh"
# The PATH= prefix below would make the shell look up `bash` in the stub
# PATH too (new-environment PATH search), so call it by resolved path.
bashbin="$(command -v bash)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

# The capabilities the check probes — keep in step with JARVIS_CLIS in
# loopd-image-check.sh.
TOOLS="git gh go ghapp-token jarvis-jobs jarvis-mq jarvis-board
jarvis-discord jarvis-notes jarvis-secrets jarvis-kanban
jarvis-stack-status jarvis-helper jarvis-swap jarvis-review"

make_stubs() { # <dir> [skip-one-tool]
  local dir="$1" skip="${2:-}" t
  mkdir -p "$dir"
  for t in $TOOLS; do
    [ "$t" = "$skip" ] && continue
    printf '#!/bin/sh\nexit 0\n' > "$dir/$t"
    chmod +x "$dir/$t"
  done
}

# A stand-in repo: the check only needs git to accept -C <dir>.
repo="$tmp/repo"
mkdir -p "$repo/.git"

run_case() { # <name> <stubs-dir> <want-rc> <expected-missing-line|- >
  local name="$1" stubs="$2" want="$3" wantline="$4" out rc
  out="$(PATH="$stubs" LOOPD_CHECK_REPO="$repo" "$bashbin" "$check" 2>&1)"
  rc=$?
  if [ "$rc" -ne "$want" ]; then
    echo "FAIL $name: exit $rc, want $want"
    fail=1
    return
  fi
  if [ "$wantline" = "-" ]; then
    if printf '%s\n' "$out" | grep -q MISSING; then
      echo "FAIL $name: unexpected MISSING line:"
      printf '%s\n' "$out"
      fail=1
    else
      echo "ok $name"
    fi
    return
  fi
  if ! printf '%s\n' "$out" | grep MISSING | grep -q "$wantline"; then
    echo "FAIL $name: $wantline not reported MISSING:"
    printf '%s\n' "$out"
    fail=1
  else
    echo "ok $name"
  fi
}

stubs_all="$tmp/stubs-all"
make_stubs "$stubs_all"
run_case "all present" "$stubs_all" 0 "-"

for skip in git gh go jarvis-jobs jarvis-review; do
  stubs="$tmp/stubs-skip-$skip"
  make_stubs "$stubs" "$skip"
  run_case "missing $skip" "$stubs" 1 "$skip"
done

exit "$fail"
