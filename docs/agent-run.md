# agent-run (coding rounds)

`agent-run` is the Discord-checkpoint loop that runs Claude Code jobs. It is a systemd service, same shape as `jarvis-chat`.

Unit: `/etc/systemd/system/agent-run.service` (repo: `systemd/agent-run.service`). User `agent`. Starts on boot.

It shares Ollama with `jarvis-chat`. That is expected.

The unit uses `Restart=always`, so a crash, kill, or Discord **Stop** (exit 0) comes back after 10s.

## Inhibit (keep it down)

1. Home script (preferred): `~/disable-agent-run` writes `/var/lib/jarvis/agent-run.disabled` and stops the unit. Undo: `~/enable-agent-run`.
2. File only: `touch /var/lib/jarvis/agent-run.disabled` then `sudo systemctl stop agent-run`. Undo: `rm` the file and `sudo systemctl start agent-run`.
3. systemd: `sudo systemctl disable --now agent-run`. Undo: `sudo systemctl enable --now agent-run`.

Install: `sudo bash scripts/install_agent_run.sh` from a checkout of this repo.

`agent-resume.service` still handles `agent-reboot` (marker `~/.config/jarvis/resume-pending`). This unit does not start while that marker exists.
