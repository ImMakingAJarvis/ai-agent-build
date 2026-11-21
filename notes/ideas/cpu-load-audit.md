# cpu-load-audit — load 35 was phantom: 33 threads leaked by Cursor-agent `rg` runs on a dead AFP/FUSE mount (fixed this round)

**Bottom line:** the host load of 34–35 was never CPU contention. It was 33 threads stuck
forever in `TASK_UNINTERRUPTIBLE` (D state), leaked by filesystem-wide `rg` searches from
Nathan's Cursor background agent, blocked on a GVFS/AFP network mount to an unreachable
Mac (`TempleOfTime.local`). The CPU itself sits ~90% idle (PSI pressure 0.00, iowait 0).
The stuck connection was aborted this round; load fell 35 → 2.3 in ~3 min and is decaying.
**No capping or scheduling of the stack is needed for games/voice — the stack uses ~2 of
16 threads.**

## What was actually happening

- 16 threads (7800X3D 8c/16t). `top`: 88–92% idle; `vmstat` r=1–2; PSI cpu/memory/io all 0.00.
  Yet loadavg 1/5/15 sat flat at 34–35 for 15+ min (flat = steady ~35 queued tasks).
- `ps -eLo` showed **33 threads in D state**, continuously. Load = R + D ≈ 2 + 33 = 35.
- All 33 belonged to **7 `rg` (ripgrep) process groups, every one a child of PID 329239 —
  a Cursor background agent worker** (`cursor-agent worker start --worker-dir
  /home/nathan/Desktop/setup`, running 13 days). Searches started Nov 19 15:52–18:52.
- Their open fds showed the searches were **filesystem-wide** (hundreds of files under
  `/mnt/win-backup`, `/sys`, and `/run/user/1000/gvfs/afp-volume:host=TempleOfTime.local,
  ...volume=The%20Giants%20Wallet/2TB drive files backup/...`).
- Kernel hung-task log (Nov 19 15:55): `task rg:NNN blocked for more than 122 seconds`,
  call trace `__do_sys_newstat → path_lookupat → d_alloc_parallel → schedule` and one in
  `fuse_lookup → fuse_lock_inode`. Mechanism: rg stat/open'd paths under the **GVFS AFP
  mount**; the FUSE request never completes because the remote Mac is unreachable (the
  afp provider blocks in the FUSE daemon), so every rg worker thread parks in D state.
  The main thread of each rg died (agent timeout/kill → zombie), but **signals cannot
  reap D-state threads** → 3–9 threads leaked per search, permanently.
- Diagnostic gotcha (kernel 7.0.0-42): the *global* counters undercount these D tasks —
  `/proc/stat` `procs_blocked` and the 4th field of `/proc/loadavg` showed 1–5 while ps
  showed 33 D threads — but the loadavg EWMA counted them correctly. That split is why
  "load 35, CPU idle" looked impossible.

## Fix applied this round (verified live)

1. Identified the hung FUSE connection: `/sys/fs/fuse/connections/{271581187,65,70}` —
   only **70** (nathan's, = gvfsd-fuse) had `waiting=1`; the ntfs-3g (root) and portal
   connections were clean.
2. `echo 1 > /sys/fs/fuse/connections/70/abort` — pending FUSE I/O completes with EIO.
3. **Verified:** D-state threads 33 → 0 within seconds; zombies 9 → 2 (the 7 rg zombies
   reaped by their parent automatically); 1-min load 35 → 3.6 in 2.5 min (5/15-min values
   decay to ~3 over the following ~15 min — exponential, predictable). gvfsd-fuse daemon
   survived and its mount still responds; Cursor agent worker intact.

## Real CPU consumers per service (10 s windows, post-fix)

| Consumer | Where | CPU | Verdict |
|---|---|---|---|
| `llama-server` | jarvis-engine (brain) | ~9–100% *while serving inference* (my own turns), idle between | expected; VRAM is the constraint (19.6/24 GB), not CPU |
| `resources` (`net.nokyan.Resources` flatpak, PID 24051) | GNOME session | **40–100% of one core, continuous for 8.7 d, 2.1 GB RAM** | app bug (busy-poll); the one real waste left |
| gnome-shell / firefox / cursor | GNOME session | ~10–18% each, spiky | normal desktop |
| `resourced` (window controller), task-runner, agent-run, plane-bridge, jarvis-watchdog | systemd | all <2% avg | fine |
| All 29 containers (rabbitmq 0.6%, nvidia-exporter 3.3% spike, rest <0.2%) | docker | ~0 | idle — the queue, not the workers, is what keeps them warm |
| `beam.smp` (dnsmasq user), sunshine, dnsmasq | host | ~1% each | fine |

## Options / next steps (for Nathan)

1. **(Done)** Abort hung FUSE connection 70 — load fixed, verified.
2. **Close or restart the `resources` app** (net.nokyan.Resources) — recovers ~0.4–1 core
   and 2 GB RAM. If Nathan wants it, restart it fresh (the 8.7-day instance is the buggy
   one); or cap it: `flatpak override --talk-name=...` won't help — simplest is
   `kill 24051` (it's a flatpak sandbox; restarts on next launch).
3. **Constrain the Cursor background agent's search scope** — the worker at
   `~/Desktop/setup` runs unscoped `rg` that walks `/mnt/win-backup`, the *network* FUSE
   mount, and `/sys`. Any search touching the dead AFP share will re-leak D threads and
   re-inflate load. Options: add a search-root/ignore config to that Cursor project, or
   stop the 13-day-old worker if it is no longer needed.
4. **Decide the fate of `TempleOfTime.local`** — if that Mac stays off, unmount the AFP
   share in Files (it will keep hanging anything that touches it, including GNOME's file
   manager) or keep the Mac on while the share is mounted.
5. **(Optional)** Report the loadavg/global-counter undercount of D tasks on
   7.0.0-42-generic to Ubuntu — cosmetic for us (load was right), but it will confuse
   every future "why is load high?" audit.

## Recommendation

Do #2 and #3 (cheap, and #3 is the actual recurrence guard); treat #4 as a housekeeping
call. Nothing in the jarvis stack needs capping or scheduling: with the brain at ~2 cores
of CPU and the rest idle, a Steam game and voice calls have all 16 threads to themselves —
the real headroom constraint remains VRAM (the existing photo swap window already handles
that), not load average.
