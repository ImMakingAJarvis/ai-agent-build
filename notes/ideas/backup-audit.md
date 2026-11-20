# backup-audit — the agent's memory is a single copy on the system disk

(Think job `think-backup-audit`, 2026-11-20. Verified live: mounts, sizes, SMART, vector graph.)

## What exists and where it is backed up

| Data | Size | Location | Backed up? |
|---|---|---|---|
| Repo (code, scripts, docs, systemd units) | 0.4G | `/home/agent/ai-agent-build` | ✅ GitHub (uncommitted worktree changes are not) |
| Agent state: job history (372 done + 81 ready, incl. full transcripts), kanban board, plans, memory.db, artifacts, photos, secrets store | 6.4G | `~/.local/share/jarvis` | ❌ single copy |
| Notes (conversation logs + agent notes) | ~1M | `notes/` in repo | ⚠️ `notes/agent/*` tracked in git; daily `notes/conversations/` persist only via the notes-only PR — today's untracked files would die with the worktree |
| Config + tokens: `agent.env`, discord/ha/jellyfin env, `plane-webhook-secret`, tls | <1M | `~/.config/jarvis` | ❌ single copy (re-provisionable but annoying; HA/Discord/Plane tokens) |
| GitHub App key, `bootstrap.env` | <1M | `/home/agent/` | ❌ single copy (reissuable from GitHub UI) |
| Benchmark results (whisper, engine benchmarks, history) | 46G | `/var/lib/jarvis/bench` | ❌ single copy — unique accumulated history, not cheaply rerunnable |
| Generated art / photo pipeline outputs | 53G | `/var/lib/jarvis/art` | ❌ (reproducible by re-running, but expensive) |
| Models: brain 16.5G + two local `.bak` copies, iGPU 2G | ~55G | `/opt/jarvis-{engine,igpu}/models` | ❌ on disk, but re-downloadable (HuggingFace) — source not written down |
| Jellyfin config + 51M demo library | 52M | `/var/lib/jarvis/jellyfin` | ⚠️ demo clips are re-downloadable; config re-provisionable |
| Steam library | 169G | `/mnt/ssd1t` | re-downloadable (and Nathan asked to clear it, 11-04) |
| HA config | — | **other box** `111.1.2.200` | out of scope — HA has a built-in backup feature, likely pointed nowhere |
| Plane (Postgres volume) | — | **other box** `111.1.2.222` | out of scope — docker volume, likely unbacked up |

**No backup mechanism of any kind exists on this box**: no cron, no systemd timer, no
script in `scripts/`, nothing in `/etc/cron.d`. `rsync` is installed; no restic/borg.
System disk `nvme1n1` (915G, 73% used) is SMART **PASSED**, but it is the single point of
failure: the agent's entire memory, config, tokens and bench history die with it.

## Backup targets available

- **`/mnt/ssd1t`** — 1TB 980 PRO, ext4, rw, **700G free** (holds 169G Steam). Different
  physical disk → survives system-disk death.
- **`/mnt/win-backup`** — 2TB 990 PRO, NTFS, **mounted ro by design**, 1.1T free. It is a
  Windows install; not a sensible Linux backup target without remounting rw.
- **NAS** — creds exist locally at `/etc/jarvis-nas.credentials` (root 600, since 11-04),
  but the share `Jarviss` **still does not exist** (asked on Discord twice; same blocker
  as the Reel library, see `notes/ideas/reel-gap.md`). The only true second-machine copy.
- **GitHub** — repo already there; a private vault repo could carry the small text state.

## Options

1. **Nightly rsync to `/mnt/ssd1t`** (minimal; ~10 min of work). One script + systemd
   timer: rsync `~/.local/share/jarvis`, `~/.config/jarvis`, repo `notes/`,
   `/var/lib/jarvis/{bench,art,jellyfin/config}` into `/mnt/ssd1t/jarvis-backup/` with
   daily rotation (keep 7 daily + 4 weekly). ~110G first run, tens of GB/month growth.
   Protects against disk failure, not against machine-level disaster (fire/theft/PSU).
2. **Second-machine copy to the NAS** (the real off-box backup). Same script, second
   destination. Blocked only on the `Jarviss` share existing — the *same* human action
   already requested for Reel; one ask unblocks both.
3. **Private "vault" GitHub repo for the small text state** (jobs.jsonl 48M, kanban,
   plans, notes, config *templates*). Second machine with zero new hardware; secrets must
   be redacted or the tarball age-encrypted before push — the secrets store and tokens
   should not sit in plain git.
4. **Models: don't back up — record the source** (HF URL + quant + ctx) in
   `notes/agent/system.md`; re-download is a 15G fetch and local `.bak`s already exist.
5. **One-line audits of the other two boxes** (HA backup target on 111.200, Plane
   Postgres volume on 111.222) as separate follow-up jobs — they are their own machines
   with their own SPOFs.

## Recommendation

Do **option 1** now (cheap, local, kills the system-disk SPOF) and **option 4** (free).
Then **option 2** the moment the `Jarviss` NAS share exists — it is the only true
off-box copy and the blocker is already known and queued in the Reel thread. Option 3 is
a nice-to-have for the small text state if NAS access keeps slipping; options 1+2 make
it redundant. Suggested first-run target set: agent state + config + notes + bench
(≈53G); include `art` (53G) if the 700G headroom matters, exclude if it should stay
minimal.
