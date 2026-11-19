# Think backlog (idle work)

The idle-work system (docs/idle-work.md) tops up a low-priority queue from
this file. One entry per line, format:

    - <slug>: <one-line description of the thinking task>

The slug becomes the job id `think-<slug>` (kebab-case, unique — the job
store is the claim ledger: a job with that id in ANY status means the
entry is taken). Entries are consumed oldest-first. When the backlog runs
out, the top-up enqueues a daily "refill" think job that brainstorms and
appends new entries, so the loop never dies.

Rules for new entries (the refill job enforces the same bar): concrete,
bounded, closable in one round — the output is a 1–2 page note in
`notes/ideas/<slug>.md`, not code. No secrets, no external side effects.

## Entries

- render-deep-dive: verify which render/GPU-sharing networks actually pay for a 4090 that runs a 27B brain 24/7 (Salad, Vast.ai, Golem, BOINC): setup, VRAM coexistence, realistic $/day, crypto-wallet needs — then update docs/render-network.md with a go/no-go recommendation
- power-economics: model this machine's 24/7 power bill (idle vs think-round vs render loads) from docs/gpu-power-measurements.md — $/month per mode, and what a night of idle work costs vs the heat it provides
- docs-drift-audit: compare docs/ (services, ports, paths, runbooks) against the live stack (jarvis-stack-status, /etc/systemd, compose files) and list every stale statement with its fix
- failed-triage: the ~60 failed agent jobs — categorize the failure modes and recommend requeue / skip / fix-first per category (note only, do not move jobs)
- stack-ideas-5: five project ideas that would make this stack more useful to Nathan and the agent, ranked by value/effort, each with a one-paragraph pitch and a first concrete step
- voice-latency-map: trace the voice-call path (wake -> chat daemon -> engine -> reply, docs/voice-call.md) and map the latency stages; where the biggest cuts would be
- reel-gap: what reel (the Plex clone, docs/reel/) is missing against its plan — the top three gaps that block a real viewing experience
- backup-audit: what on this box is not backed up anywhere (the repo is on GitHub; what about ~/.local/share/jarvis, notes/, models, HA config, Plane data?) and a minimal backup plan
- dashboard-ops-ideas: five dashboard improvements that would make operating this stack easier (queue, engines, jobs, benchmarks, kanban)
- moe-scout: which currently-released open models would fit the 4090's 24.5 GB at Q4 and beat the current 27B brain, and which to bench next with the existing suite
- digest-design: design the ideal morning digest for Nathan — what it should contain, what to cut, how long, in what order — as a spec for scripts/think_digest.py
- ha-automation-ideas: five Home Assistant automations this house could use (jarvis-ha sensors, lights, climate, GE/SmartHQ appliances), each with the trigger and the action
