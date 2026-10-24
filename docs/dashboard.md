# Dashboard

Web UI for queue and job history. systemd unit, same shape as `jarvis-chat` and `agent-run`.

- Unit: `/etc/systemd/system/jarvis-dashboard.service` (repo: `systemd/jarvis-dashboard.service`)
- Binary: `/usr/local/bin/jarvis-dashboard` (`cmd/dashboard`)
- User: `agent` (so it reads the agent job store, not Nathan's empty one)
- URL: **http://111.1.2.57:8080/** (or http://localhost:8080 on the box)
- Polls `GET /html/dashboard` as HTML. `/api/state` stays JSON. The old :8081 shim is not needed for this build.
- Each Agent Job and Recent Job row has a **Log** link to `/jobs/<id>`: thoughts, commands, and outcomes from that job's `transcript.txt` (or `output.txt` / `jobs.jsonl` summary if that is all that exists). Empty jobs say “No log for this job.” Missing IDs 404. Running jobs refresh every 5s. Traces stay in the existing job store; the dashboard does not keep a second copy.
- Unbounded lists (approvals, agent jobs, recent jobs) first-paint **25 rows** and load more on demand. Older denied jobs stay in `jobs.jsonl`; the UI does not delete them.

Does not require `agent-run`.

## Inhibit

1. File: `touch /var/lib/jarvis/dashboard.disabled` then reboot, or `sudo systemctl stop jarvis-dashboard`. Undo: `rm` the file and `sudo systemctl start jarvis-dashboard`.
2. systemd: `sudo systemctl disable --now jarvis-dashboard`.

Install: `sudo bash scripts/install_dashboard.sh` from a checkout of this repo.

D0.3 should reuse this unit and only add `jarvis-worker.service`.
