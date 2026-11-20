# reel-gap — Reel is a scaffold: no library, no playback, no front door (2026-11-20)

## What I found

Reel (the Plex clone, plan `~/.local/share/jarvis/plans/plan-jarvis-build-plan-open-s-1793478730/`,
16-job series `plan-jarvis-build-plan-open-s-1793478730-1..16`):

| Job | State | What exists today |
|---|---|---|
| -1 Jellyfin baseline | done | `jarvis-jellyfin` up (12.4, NVENC verified) — but on a **3-clip demo library** |
| -2 Gate 1 | done | scratch/Go, MIT, 4090 box, multi-user, TestFlight |
| -3 Go scaffold | done | `reel/`: reeld binary, SQLite, OpenAPI v0 with **6 ops** (health, libraries, listItems/getItem), CI, Docker |
| -4 scanner | **failed** | nothing — see below |
| -5..-16 | ready, untouched | metadata, web UI, direct play, transcode, profiles, apps, Tailscale, v1 |

The Reel server today can list libraries and items but **cannot stream a single byte of
video**. The family's only working "Reel" is stock Jellyfin on the demo clips.

Two facts that shape everything:

1. **Job -4 failed mechanically, not substantively.** Its transcript is one turn:
   `API Error: Connection refused (ECONNREFUSED)` — the brain was down at round start, so
   the scanner was never even started. Requeueing it costs nothing; there is no work to
   salvage or reconcile.
2. **The real media is still unreachable.** The NAS `Jarviss` share does not exist (checked
   2026-11-06, docs/reel/jellyfin-baseline.md) and the existing `Plex` share is access-denied
   for the NAS user. NAS creds already exist in `/etc/jarvis-nas.credentials` and the
   read-only CIFS mount pattern is in `bootstrap.sh` — only the share itself is missing.
   Until then, even a perfect scanner scans 3 demo clips, and the Jellyfin baseline (the
   day-1 working system) stays on the same demo.

Plan's v1 bar: scan + metadata/artwork, direct play + GPU transcode, resume/watched/
profiles, Tailscale.

## Top three gaps blocking a real viewing experience

**1. There is no library — scanner (job -4) + real media (human action).**
No scan, no items; every downstream job (playback, metadata, UI) would be demo-grade.
Agent half: requeue -4 (self-contained: test library, filename parsing, queue-based scan,
watchdog pause — all specified). Human half: create the `Jarviss` share (or grant read on
`Plex`) — this is the single biggest external dependency in the whole plan and it blocks
Reel *and* the Jellyfin baseline simultaneously.

**2. Nothing plays — no streaming path at all (jobs -7, -8).**
-7: direct play + resume — no stream route exists in `reeld` today. -8: NVENC HLS
transcoding — required (not nice-to-have) because the family's real devices (Apple TV,
iPhone, phones) can't direct-play H.265 MKV; the plan's v1 is "direct play first,
transcode when a client can't play." Useful references already in the repo:
`docs/reel/jellyfin-baseline.md` verified NVENC HLS on this box (single 720p transcode
safe with ~2.9 GB VRAM headroom over the brain) and documents 12.x's session-bound resume
semantics — a design reference for Reel's own, simpler resume contract (spec first, per
the OpenAPI rule).

**3. No front door — no metadata, no UI (jobs -5, -6).**
-5: TMDB metadata + artwork (a **TMDB API key is not in `jarvis-secrets` yet** — needs
Nathan, or a free key registered). -6: web UI — browse, search, admin, browser playback.
Without these, the family can't find or click anything; the "experience" is a REST API.
(v1 also wants -9 profiles/watched-state/auth and -15 Tailscale, but those are
second-order: one household can watch before profiles, and LAN viewing works before
remote.)

## Options / next steps

1. **Requeue -4 now** — mechanical failure, zero progress lost; driver local, fits one
   round (test library + unit tests + queue E2E).
2. **Ask Nathan on Discord for the NAS share** (create `Jarviss` or grant read on `Plex`).
   One human action unblocks both Reel's real library and the Jellyfin baseline's real
   library.
3. **Get a TMDB key into secrets** before -5 starts (small ask, or agent registers a
   free key if Nathan prefers).
4. **Sequence the rest on the critical path: -7 → -8 → -5 → -6.** Playback endpoints
   first (the UI is a thin click wrapper over them), metadata before the UI so the UI
   ships with titles/artwork instead of filenames, transcode before UI so the first
   family viewing works on a TV, not just a browser.
5. **Optional cheap gate** after -7: curl-level watchability check — a scanned test movie
   serves 206 with a valid ftyp head, a forced HLS transcode produces an m3u8 the
   browser plays. Mirrors the verification record style in jellyfin-baseline.md.

## Recommendation

Do 1 and 2 in parallel right away (agent requeues -4, Discord ask for the share goes out
in the same breath), then run -7 → -8 → -5 → -6 in order. Hold the apps (-11..-14) and
Tailscale (-15) until the playback API is stable — the plan itself gates apps on "server
API is stable," and every app job against a moving streaming contract is rework. The
NAS share is the only truly external dependency; everything else on the critical path is
agent-buildable. If the share stays absent, the best available "real viewing experience"
is a small real-media folder on this box mounted read-only into the demo path — worth
asking about only if the NAS stays blocked a week or two.
