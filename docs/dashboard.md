# Dashboard

Web UI for queue and job history. systemd unit, same shape as `jarvis-chat` and `agent-run`.

- Unit: `/etc/systemd/system/jarvis-dashboard.service` (repo: `systemd/jarvis-dashboard.service`)
- Binary: `/usr/local/bin/jarvis-dashboard` (`cmd/dashboard`)
- User: `agent` (so it reads the agent job store, not Nathan's empty one)
- URL: **http://111.1.2.57:8080/** (or http://localhost:8080 on the box)
- Polls `GET /html/dashboard` as HTML. `/api/state` stays JSON. The old :8081 shim is not needed for this build.
- Each Agent Job and Recent Job row has a **Log** link to `/jobs/<id>`: thoughts, commands, and outcomes from that job's `transcript.txt` (or `output.txt` / `jobs.jsonl` summary if that is all that exists). Empty jobs say “No log for this job.” Missing IDs 404. Running jobs refresh every 5s. Traces stay in the existing job store; the dashboard does not keep a second copy.

Does not require `agent-run`.

## Voice chat link

The subtitle bar's **Voice Chat** link is built from the request's `Host`
header (`voiceChatURLFromHost`), never hardcoded:

- `localhost`/`127.0.0.1`/`::1` → `http://<host>:8100/voice/chat` — localhost
  is a secure context, so the mic works and no cert warning appears.
- any other host → `https://<host>:8443/voice/chat` — browsers only expose
  the microphone in secure contexts, so remote LAN hosts must use the voice
  service's HTTPS listener (self-signed cert; see `scripts/gen_voice_cert.sh`).
  The plain-HTTP `:8100` port stays up for the Home Assistant integration.
- If the page was opened over plain HTTP anyway, it shows a banner pointing
  at the HTTPS URL instead of a bare "access denied" when the mic fails.

## Talking to the worker (interrupt without stopping)

The **Talk to the worker** box on the dashboard sends the agent a message
the way you interrupt Claude or Cursor: the running process is never
stopped or restarted — the message is injected into the agent's context.

- `POST /api/owner-message` stores the message in the agent inbox
  (`$JARVIS_DATA_DIR/inbox/`, default `~/.local/share/jarvis/inbox/`).
  `jarvis-jobs message [TEXT]` does the same from the shell.
- `jarvis-jobs run` exports `JARVIS_JOB_ID` into the agent session, and
  the hook `scripts/owner-message-hook.sh` (wired in `.claude/settings.json`)
  checks the inbox on every loop step:
  - **PreToolUse** — a waiting message is printed as `additionalContext`,
    so the model sees it before its next tool call;
  - **Stop** — a message that arrived just as the round was ending blocks
    the stop (exit 2) and feeds the message back, so the model gets one
    more turn with it.
- Messages sent while no job is running stay in the inbox; the runner
  folds them into the next round's prompt ("Messages from me sent while
  the agent was idle").
- The Agent Jobs section shows how many messages are waiting, and
  delivered messages are kept under `inbox/delivered/` (last 200).
- Other claude sessions in this repo (no `JARVIS_JOB_ID`) never touch the
  inbox, so the Discord chatbot cannot swallow a message meant for the
  worker.

Discord still has its old interrupt behavior (pause + `jarvis-reply`): a
DM to the bot pauses the job when the chat slot is busy. The dashboard
box is the non-pausing path.

## Inhibit

1. File: `touch /var/lib/jarvis/dashboard.disabled` then reboot, or `sudo systemctl stop jarvis-dashboard`. Undo: `rm` the file and `sudo systemctl start jarvis-dashboard`.
2. systemd: `sudo systemctl disable --now jarvis-dashboard`.

Install: `sudo bash scripts/install_dashboard.sh` from a checkout of this repo.

D0.3 should reuse this unit and only add `jarvis-worker.service`.
