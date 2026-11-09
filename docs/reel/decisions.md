# Reel — Gate 1 decisions

Answered by Nathan on Discord, 2026-11-06. Source plan: "Jarvis Build Plan:
Open Source Plex Clone" (2026-10-30). Work board: Plane project `RCL`
(epic RCL-1, http://111.1.2.222:8088/jarvis/browse/RCL-1).

| # | Decision | Answer | Plan's recommendation |
|---|----------|--------|-----------------------|
| 1 | Build approach | **Build from scratch (Go)** | scratch (matches) |
| 2 | Server location | **4090 box** | 4090 box (matches) |
| 3 | Apple distribution | **Paid Apple Developer Program + TestFlight** | paid (matches) |
| 4 | License | **MIT** | GPL-3.0 or MIT; GPL only if Jellyfin code is reused |
| 5 | Users | **Nathan + family/tenants** | multi-user |
| 6 | Name | **Keep "Reel"** | keep |

## Notes

- **2 (server):** Nathan's answer was conditional: "Is it possible to do
  transcoding on the iGPU? If so, 4090 box; otherwise Synology." Checked
  2026-11-06: the Ryzen iGPU (Raphael, `/dev/dri/renderD129`) encodes H.264
  through VAAPI (`ffmpeg -c:v h264_vaapi`, 1080p test OK; `hevc_vaapi` and
  `av1_vaapi` are compiled in, not tested). So: 4090 box; the iGPU is a
  spare transcoder. Nathan was told on Discord and can overrule.
- **1 + 4 (scratch + MIT):** because Reel is built from scratch, **no
  Jellyfin code may be copied into the repo** (that would force GPL-3.0).
  Jellyfin stays the day-1 baseline system only (docs/reel/jellyfin-baseline.md);
  read its API behaviour, do not paste its source.
- **3 (Apple):** an Apple Developer account must be created and paid for by
  Nathan before the iPhone/Apple TV jobs can ship builds via TestFlight.
- **5 (users):** job -9 (multi-account) and job -15 (network reach for the
  family's/tenants' networks) stay in scope as planned.
- **Queue:** direction is scratch, so jobs -3..-16 are unchanged.
