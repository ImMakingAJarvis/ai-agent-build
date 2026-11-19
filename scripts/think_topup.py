#!/usr/bin/env python3
"""think_topup — keep the think queue topped up (docs/idle-work.md).

The idle-work system: when the real job queue drains (nights, quiet days),
the agent loop should still have work. This top-up keeps FLOOR think jobs
(tag `think`, class background, lowest priority) ready, sourced from
config/think-backlog.md. Think jobs are strictly last-resort: every real
job outranks them, so real work always preempts thinking.

Dedupe is the job store itself: a backlog entry becomes job id
`think-<slug>`, and the store refuses (idempotently) to add a job whose id
already exists in ANY status — done jobs keep their ids, so a finished
thinking task is never re-queued.

When the backlog runs out, one refill job per UTC day brainstorms and
appends new entries (the self-replenishing part of the loop).

Env: JARVIS_REPO_DIR (repo with config/think-backlog.md), JARVIS_DATA_DIR
(job store; default ~/.local/share/jarvis), THINK_FLOOR (default 3),
THINK_PRIORITY (default -100).

Runs as a oneshot from the jarvis-think-topup timer (15 min). A missing
backlog or repo is a logged no-op, never a failed unit — before the
PR that ships this file is merged, the timer ticks and does nothing.
"""

import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone

REPO = os.environ.get("JARVIS_REPO_DIR", "/home/agent/ai-agent-build")
BACKLOG = os.path.join(REPO, "config", "think-backlog.md")
DATA_DIR = os.environ.get("JARVIS_DATA_DIR",
                          os.path.expanduser("~/.local/share/jarvis"))
FLOOR = int(os.environ.get("THINK_FLOOR", "3"))
PRIORITY = int(os.environ.get("THINK_PRIORITY", "-100"))
ACTIVE = ("ready", "claimed", "running", "waiting")


def log(msg):
    print(f"think-topup: {msg}", flush=True)


def job_dir(jid):
    return os.path.join(DATA_DIR, "jobs", jid)


def job_exists(jid):
    return os.path.isdir(job_dir(jid))


def count_active_think():
    """Think-tagged jobs in an active state (the queue's think depth)."""
    n = 0
    jobs = os.path.join(DATA_DIR, "jobs")
    if not os.path.isdir(jobs):
        return n
    for d in os.listdir(jobs):
        sj = os.path.join(jobs, d, "state.json")
        jj = os.path.join(jobs, d, "job.json")
        try:
            with open(sj) as f:
                if json.load(f).get("status") not in ACTIVE:
                    continue
            with open(jj) as f:
                if "think" in (json.load(f).get("tags") or []):
                    n += 1
        except (OSError, ValueError):
            continue
    return n


def backlog_entries():
    """(slug, description) pairs, file order; only well-formed lines."""
    out = []
    if not os.path.isfile(BACKLOG):
        return out
    with open(BACKLOG) as f:
        for line in f:
            m = re.match(r"^- ([a-z0-9][a-z0-9-]*): (.+)$", line.strip())
            if m:
                out.append((m.group(1), m.group(2).strip()))
    return out


def think_prompt(slug, desc, jid):
    return (
        f"This is a THINK job (idle-time work — docs/idle-work.md): the real\n"
        f"queue was empty, and you are keeping the machine useful. Think jobs\n"
        f"ship no code and open no PRs.\n\n"
        f"Task: {desc}\n\n"
        f"Do:\n"
        f"1. Search the vector graph before grepping the filesystem:\n"
        f"   curl -s -X POST localhost:8101/search -d '{{\"query\":\"...\",\"k\":5}}'\n"
        f"2. Think it through; read whatever in the repo/docs/notes you need.\n"
        f"3. Write notes/ideas/{slug}.md — 1-2 pages: what you found, 2-5\n"
        f"   concrete options or next steps, and your recommendation. First\n"
        f"   line: \"# {slug} — <short title>\". Working tree only; never\n"
        f"   commit notes/** into a PR.\n"
        f"4. jarvis-notes add done \"think {slug}: <one-line outcome>\"\n"
        f"5. Do NOT queue follow-up jobs yourself — the 08:00 digest\n"
        f"   (scripts/think_digest.py) surfaces your note to Nathan and he\n"
        f"   decides what becomes real work.\n"
        f"6. Write the conversation note, then: jarvis-jobs finish {jid} 0\n"
    )


def refill_prompt(jid, day):
    return (
        "This is a THINK job (idle-time work — docs/idle-work.md): the think\n"
        "backlog (config/think-backlog.md) ran out and the loop must not die.\n\n"
        f"Brainstorm 5-10 NEW thinking tasks and append them to\n"
        "config/think-backlog.md under the existing '## Entries' section, one\n"
        "line each: \"- <slug>: <one-line description>\". Slugs are kebab-case\n"
        f"and must be unique across the whole file (check first; a slug that\n"
        "already has a think-<slug> job in the store is taken). Same bar as\n"
        "the existing entries: concrete, bounded, closable in one round whose\n"
        "output is a 1-2 page note — not code, no secrets, no external side\n"
        "effects. Favor things that would make this stack more useful or\n"
        "find things worth doing that nobody has queued.\n\n"
        f"Then write notes/ideas/refill-{day}.md (the themes you considered\n"
        "and why you picked what you picked), jarvis-notes add done\n"
        f"\"think refill {day}: appended N new backlog entries\", the\n"
        f"conversation note, and: jarvis-jobs finish {jid} 0\n"
    )


def add_job(jid, title, prompt):
    r = subprocess.run(
        ["jarvis-jobs", "add",
         "-id", jid, "-title", title, "-instructions", prompt,
         "-class", "background", "-priority", str(PRIORITY),
         "-tags", "think", "-driver", "local", "-effort", "low",
         "-source", "think"],
        capture_output=True, text=True)
    if r.returncode != 0:
        log(f"add {jid} failed: {r.stderr.strip() or r.stdout.strip()}")
        return False
    log(f"queued {jid}")
    return True


def main():
    if not os.path.isdir(DATA_DIR):
        log(f"no job store at {DATA_DIR}; nothing to do")
        return 0
    active = count_active_think()
    if active >= FLOOR:
        log(f"{active} think job(s) active (floor {FLOOR}); nothing to do")
        return 0
    want = FLOOR - active
    entries = [e for e in backlog_entries()
               if not job_exists(f"think-{e[0]}")]
    if not os.path.isfile(BACKLOG):
        log(f"no backlog at {BACKLOG} (not merged yet?); nothing to do")
        return 0
    added = 0
    for slug, desc in entries[:want]:
        if add_job(f"think-{slug}", f"think: {slug}",
                   think_prompt(slug, desc, f"think-{slug}")):
            added += 1
    if added < want and not entries:
        # Backlog exhausted: one refill brainstorm per UTC day.
        day = datetime.now(timezone.utc).strftime("%Y%m%d")
        mid = f"think-refill-{day}"
        if not job_exists(mid):
            if add_job(mid, f"think: refill backlog {day}",
                       refill_prompt(mid, day)):
                added += 1
    log(f"done: {active + added} think job(s) active")
    return 0


if __name__ == "__main__":
    sys.exit(main())
