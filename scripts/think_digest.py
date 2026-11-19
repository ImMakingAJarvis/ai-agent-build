#!/usr/bin/env python3
"""think_digest — the morning report on overnight think work.

Runs daily at 08:00 (jarvis-think-digest.timer, Persistent=). Collects the
think notes written since the last digest (notes/ideas/*.md newer than the
marker) plus the count of think jobs that finished or failed in the window,
and DMs Nathan one short message. With nothing new it says nothing — an
idle night is not an event.

The marker is ~/.local/state/jarvis/think-digest.last (epoch seconds); a
missing marker means "since 24 h ago". Env: JARVIS_REPO_DIR (where
notes/ideas/ lives), JARVIS_DATA_DIR (job store), THINK_DIGEST_STATE
(marker path, for tests).
"""

import glob
import json
import os
import subprocess
import sys
import time

REPO = os.environ.get("JARVIS_REPO_DIR", "/home/agent/ai-agent-build")
IDEAS = os.path.join(REPO, "notes", "ideas")
DATA_DIR = os.environ.get("JARVIS_DATA_DIR",
                          os.path.expanduser("~/.local/share/jarvis"))
MARKER = os.environ.get("THINK_DIGEST_STATE",
                        os.path.expanduser("~/.local/state/jarvis/think-digest.last"))
MAX_NOTES = 8


def read_marker(now):
    try:
        with open(MARKER) as f:
            return float(f.read().strip())
    except (OSError, ValueError):
        return now - 86400


def notes_since(marker):
    out = []
    if not os.path.isdir(IDEAS):
        return out
    for f in sorted(glob.glob(os.path.join(IDEAS, "*.md"))):
        try:
            if os.path.getmtime(f) > marker:
                out.append(f)
        except OSError:
            continue
    return out


def first_heading(path):
    try:
        with open(path) as f:
            for line in f:
                if line.startswith("# "):
                    return line[2:].strip()
    except OSError:
        pass
    return ""


def think_results_since(marker):
    """(done, failed) counts for think- jobs whose state.json changed after marker."""
    done = failed = 0
    jobs = os.path.join(DATA_DIR, "jobs")
    if not os.path.isdir(jobs):
        return done, failed
    for d in os.listdir(jobs):
        if not d.startswith("think-"):
            continue
        sj = os.path.join(jobs, d, "state.json")
        try:
            if os.path.getmtime(sj) <= marker:
                continue
            with open(sj) as f:
                st = json.load(f).get("status")
            if st == "done":
                done += 1
            elif st == "failed":
                failed += 1
        except (OSError, ValueError):
            continue
    return done, failed


def main():
    now = time.time()
    marker = read_marker(now)
    notes = notes_since(marker)
    done, failed = think_results_since(marker)
    if not notes and not done and not failed:
        print("think-digest: nothing new; no message", flush=True)
        return 0

    lines = []
    for f in notes[:MAX_NOTES]:
        title = first_heading(f) or os.path.basename(f)[:-3]
        lines.append(f"- {title}  (notes/ideas/{os.path.basename(f)})")
    if len(notes) > MAX_NOTES:
        lines.append(f"- …and {len(notes) - MAX_NOTES} more in notes/ideas/")

    head = f"Overnight thinking: {done} think job(s) done"
    if failed:
        head += f", {failed} FAILED (check the Agent Jobs page)"
    msg = head
    if lines:
        msg += "\n" + "\n".join(lines)
    msg += "\nWhich of these should become real work?"

    r = subprocess.run(["jarvis-discord", "notify", msg],
                       capture_output=True, text=True)
    if r.returncode != 0:
        print(f"think-digest: notify failed: {r.stderr.strip()}", file=sys.stderr,
              flush=True)
        return 1
    os.makedirs(os.path.dirname(MARKER), exist_ok=True)
    with open(MARKER, "w") as f:
        f.write(str(now))
    print(f"think-digest: sent ({len(notes)} notes, {done} done, {failed} failed)",
          flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
