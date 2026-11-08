# Reel P0 — Stock Jellyfin baseline

Day-1 working Plex alternative: stock Jellyfin in the jarvis compose stack,
NVENC on the 4090, media mounted read-only. This is the reference/fallback
for the whole Reel plan (see the plan in
`~/.local/share/jarvis/plans/plan-jarvis-build-plan-open-s-1793478730/`).

## What runs where

| Piece | Where |
|---|---|
| Jellyfin 12.4 (`jellyfin/jellyfin:12.4`, container `jarvis-jellyfin`) | this box (RTX 4090 for NVENC), compose service `jellyfin` in `docker-compose.yml` |
| Web UI / API | `http://111.1.2.222:8096` (port 8096 published) |
| Config | `/var/lib/jarvis/jellyfin/config` (bind → `/config` in container) |
| Media (demo library) | `/var/lib/jarvis/jellyfin/demo` (bind → `/media`, **read-only**) |
| Admin credentials | `/home/agent/.config/jarvis/jellyfin.env` (chmod 600, **never in the repo**) |

The service has `restart: unless-stopped` and is part of the `ai-agent-build`
compose project, so `jarvis-stack.service` brings it up at boot (verified:
stopped the container, ran the boot unit's exact
`docker compose -p ai-agent-build -f .../docker-compose.yml up -d`, came back
healthy).

## Libraries

- **Movies** — `/media/movies` (container path; host
  `/var/lib/jarvis/jellyfin/demo/movies`)
- **TV** — `/media/tv` (host `.../demo/tv`, layout
  `Show/Season 01/Show - S01E01 - Title.mp4`)

**Currently a demo library** (3 synthetic 720p clips generated with ffmpeg —
`Reel Baseline Demo (2026)` + `Reel Baseline Show` S01E01/E02), because the
NAS share does not exist yet: Nathan named it `Jarviss` (Discord,
2026-11-04); as of 2026-11-06 it is still not created, and the existing
`Plex` share is access-denied for the NAS user (checked with `smbclient`)
— the NAS creds themselves authenticate fine. When the share exists:

1. Mount it read-only (see `bootstrap.sh` CIFS pattern; creds already in
   `/etc/jarvis-nas.credentials`).
2. In `docker-compose.yml`, add a second (or replacement) read-only bind for
   the movie/TV subfolders and `docker compose up -d jellyfin`.
3. Trigger a scan: `POST /Library/Refresh` (or wait for the library monitor,
   60 s delay).

The agent never deletes or renames media files; mounts are `ro`.

## Hardware transcoding (NVENC)

- Toggle in 12.x: `config/config/encoding.xml` on the host (a second
  `config/` level — `/config/config/encoding.xml` in the container) —
  `<HardwareAccelerationType>nvenc</HardwareAccelerationType>`
  (11.x's `jellyfin.xml` HardwareAcceleration block is gone in 12.x).
  Edit while the container is stopped, then start it.
- Verified 2026-11-04: a forced HLS transcode produced an ffmpeg command
  with `-init_hw_device cuda=cu:0 ... -codec:v:0 h264_nvenc -preset p1`
  and exited 0.
- **VRAM budget:** the llama.cpp brain holds ~21.6 GB of 24.5 GB (~2.9 GB
  headroom). A 720p NVENC session adds negligible VRAM (measured: no
  visible delta). Single transcodes are safe. If multiple concurrent
  transcodes ever push the box OOM, set
  `HardwareAccelerationType` back to `none` (CPU transcoding still works —
  32-core box) and note it here.

## 12.x API notes (gotchas found the hard way)

- No `/api` prefix — routes are at the root (`/System/Info/Public`,
  `/Users/AuthenticateByName`, `/Library/VirtualFolders`, ...).
- Auth is **not** `X-MediaBrowser-Token` (legacy auth is off by default).
  Clients identify via a custom header, and the token goes in the same
  header:

  ```
  Authorization: MediaBrowser Client="Jellyfin Web", Device="agent-box", DeviceId="<uuid>", Version="12.4.0", Token="<AccessToken>"
  ```

  Login: `POST /Users/AuthenticateByName` with `{"Username":..., "Pw":...}`
  plus that header (without `Token=`). The token is persisted per device in
  the server DB, so the same DeviceId keeps working.
- Library creation: `POST /Library/VirtualFolders?name=Movies&collectionType=movies&paths=/media/movies&refreshLibrary=true`
  (query params, not a body).
- Streaming: `GET /Videos/{id}/main.m3u8?...` for HLS;
  `GET /Videos/{id}/stream.mp4?static=true` for direct play.
- Played / unwatched (per-user, **verified 11-06**):
  - Mark played: `POST /Users/{uid}/PlayedItems/{id}?isPlayed=true`
  - Unwatch: `DELETE /Users/{uid}/PlayedItems/{id}` — **not**
    `POST ...?isPlayed=false`, which is a silent no-op in 12.x (returns
    200, leaves the item played). This is why a naive "reset" loop can
    get an item stuck `Played:true`.
- Play commands (session-attached — every call needs a real session id
  from `GET /Sessions` matched by `DeviceId`; a random UUID is a 404):
  - `POST /Sessions/{sid}/Playing?playCommand=PlayNow` — the command
    binds from the **query string**, not the body (only `PlayNow` /
    `PlayShuffle` are valid there; Pause/Seek/Stop/... go to
    `POST /Sessions/{sid}/Playing/{command}`). The 4-segment
    `POST /Sessions/{sid}/Playing` with no query is a 400 in 12.x.
  - Mid-playback progress / resume position is attached to a **real
    active session** in 12.x: a bare session-less REST POST to the
    3-segment `POST /Sessions/Playing/Progress` returns 204 but does
    not persist a mid-position (it marks the item played). Confirm the
    exact progress-report binding from a real client (Web/Android)
    capture when building the Reel server (job -7).

## Stopping it

```
docker stop jarvis-jellyfin              # stop now (stays stopped after boot? no — unless-stopped)
docker compose -f docker-compose.yml stop jellyfin   # same
```

To keep it down across reboots: `docker update --restart=no jarvis-jellyfin`
(or remove the service from compose). To remove entirely:
`docker compose -f docker-compose.yml stop jellyfin && docker compose -f docker-compose.yml rm jellyfin`,
then delete the `jellyfin:` block from `docker-compose.yml` and
`rm -rf /var/lib/jarvis/jellyfin` (config + demo media).

## Verification record (2026-11-04)

- Browse: 1 movie + 1 series (2 episodes) returned by the Items API (the
  same endpoints the Web UI uses).
- Direct play: `/Videos/{id}/stream.mp4?static=true` → 206 `video/mp4`,
  valid ftyp/isom/avc1 head, for both the movie and an episode.
- Resume: play-state cycle (Playing → Progress @45 s → Resume list) showed
  the series at 50 % played; stop at full length marked the episode
  `Played: true`.
- NVENC: see above.
- Boot: see "What runs where".

## Re-verification (2026-11-06)

Re-ran live on the box (same image / config / container — nothing changed
since 11-04):
- Compose: healthy in the `ai-agent-build` project, `restart:
  unless-stopped`, media bind `:ro`, nvidia runtime; v12.4.0.
- Browse: Movies + TV virtual folders; 7 items (1 movie / 1 series /
  2 episodes) via the Items API.
- Direct play: 206 `video/mp4`, valid ftyp/isom head, for the movie and
  an episode.
- NVENC: `config/config/encoding.xml` → `nvenc` (path corrected above).
- Resume: verified in the real Web UI on 11-04 (see above). A 11-06
  synthetic REST re-test exercised the 12.x play-state endpoints (all
  204) but did not persist a position — 12.x attaches progress to a real
  active playback session (what the actual Web/Android clients report),
  so a session-less harness can't drive it. Real clients are unaffected.
- NAS: the `Jarviss` share still does not exist (see Libraries) → the
  flagged demo library is the in-use state, per the job's fallback clause.
