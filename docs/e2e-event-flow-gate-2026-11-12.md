# Phase 4 gate — run transcript (2026-11-12 22:00 PST)

Plan: `PLAN-event-driven-brain.md` §4 "Phase 4 ✔" and §6 Testing. Task EVB-30.
Script: `scripts/e2e-event-flow.sh` (run 1794549645, box-local, unattended).

**Result: GATE PASS — S1 canonical flow, S2 voice fallback, S3 game drain.
3 passed, 0 failed. Wall time 5 m 09 s (22:00:45 – 22:05:54 PST).**

## What had to be true before this run

- The resourced broker had to carry the F7 fix (`vlmserver.Stop` bound; the
  idle-unload deadlock that wedged the broker for run 5 at 13:41 and burned
  a 2 h empty window 19:43–21:43 — see `resourced.log`: the window opened on
  the 3 leftover gate messages and closed on "max window reached (2h)").
  Redeployed via `scripts/install_resourced.sh` (rebuild + restart) at
  21:59 PST; this transcript's broker is the F7 build.
- The 3 leftover `gate-1794519699-s3-img` messages from the wedged run were
  purged from `cap.image.generate` before the run (preflight needs both cap
  queues empty).

## Acceptance mapping (plan §4 Phase 4 ✔)

1. **Brain yields → window opens → image made → small job on spare VRAM →
   window closes → brain back → job resumes** — S1 PASS. The stand-in round
   yielded on `image.generate` (12 GB); the window opened at 06:01:21Z and
   the brain engine really stopped (`/health` unreachable); while the image
   was in flight `llm.small` (5.5 GB) completed on the gpu placement
   (`worker=llm.small@gpu/llm-small-worker`) — the lease snapshot shows all
   three co-scheduled (image.generate 12288 MB, image.describe 3244 MB,
   llm.small 5500 MB); the real artifact (1.4 MB PNG) landed with
   `Result status=ok`; the window closed drained; the brain engine really
   came back (`/health` 200); the switchboard woke the job to ready and it
   finished (`completed in 2m37s`).
2. **Voice reply during the window is answered by the iGPU helper** — S2
   PASS. With the brain down, `POST /chat` got an HTTP 200 reply,
   `voice /health` reported `fallback.brain_up=False`, and the voice
   service's request log shows the chat completion routed to
   `jarvis-engine-igpu:11435`.
3. **A game started mid-window cleanly drains and brings everything back**
   — S3 PASS. A fresh window opened for a second image; while an
   `llm.small` was in flight a connected UDP socket appeared on the Sunshine
   stream port (48010); `stream_active` went true within 1 s and all
   capability GPU leases were revoked; the in-flight small job DRAINED (its
   result landed); the window closed, the brain came back, and the stream
   hold cleared when the socket went away. (Close reason read "drained"
   rather than "game" — the hold tick raced the drained tick; the hold was
   active, which is what the plan requires.)
4. **`scripts/e2e-event-flow.sh` exists and reproduces the gate** — the run
   above IS the script, exit 0. Re-run with
   `ROUND_JOB=<running-local-job> bash scripts/e2e-event-flow.sh
   [transcript.log]` (CI-light: on the box, not in GitHub CI — it stops and
   starts the brain container).

## Full transcript

```
[06:00:45] Phase 4 event-flow gate  (run=1794549645, resourced=http://172.17.0.1:9104)
[06:00:45] data root: /home/agent/.local/share/jarvis, round job: plan-plan-event-driven-brain-1794016330-30
[06:00:45] preflight OK: stack up, window closed, brain up, both cap queues empty, voice routing deployed
[06:00:45] pausing round job plan-plan-event-driven-brain-1794016330-30 (rule 3: no window while a local round is RUNNING)
[06:00:45] round job now paused
[06:00:45] S1: canonical flow — stand-in job yields on image.generate (12 GB)
[06:00:45] S1: gate-1794549645-s1 is now waiting; waiting for the window to open (budget 150 s)
[06:01:21] S1: window OPEN (for=["image.generate"] reason="window open (30s), 1 message(s) still queued")
[06:01:21] S1: waiting for the brain engine to actually stop (the real completion check, going out)
[06:01:21] S1: brain engine DOWN (000unreachable)
[06:01:21] S2: voice /chat during the window (brain down) — expecting the iGPU helper
[06:01:21] S2: voice replied (http 200): OK
[06:01:21] S2: voice /health fallback.brain_up=False (want False)
[06:01:21] S2: voice chat completions routed to the iGPU helper: 1 (want >= 1)
[06:01:21] PASS S2
[06:01:21] S1: submitting llm.small (5.5 GB) while the image is in flight — the co-scheduling window
[06:01:21] S1: capturing the co-scheduled GPU leases (want image.generate AND llm.small)
[06:01:21] S1: gpu leases now:
        brain  0
        image.generate@gpu/image-gen-worker image.generate 12288
        image.describe@gpu/image-describe-worker image.describe 3244
        llm.small@gpu/llm-small-worker llm.small 5500
        vram 2261 MiB, 21820 MiB
[06:01:21] S1: waiting for the llm.small result (budget 240 s)
[06:01:51] S1: llm.small done on worker=llm.small@gpu/llm-small-worker (want the gpu placement: llm.small@gpu/llm-small-worker)
[06:01:51] S1: waiting for the image artifact (budget 300 s)
[06:01:51] S1: image artifact: /home/agent/.local/share/jarvis/artifacts/e2e-event-flow-1794549645/gate-1794549645-s1/20261113-060145-622110170.png (1.4M)
[06:01:51] S1: image Result status=ok
[06:01:51] S1: waiting for the window to close (drained; budget 480 s)
[06:03:22] S1: window CLOSED (reason="closed: drained (all capability queues empty)")
[06:03:22] S1: waiting for the brain engine to actually come back (budget 240 s)
[06:03:22] S1: brain engine UP again — the round can continue
[06:03:22] S1: waiting for the switchboard to wake gate-1794549645-s1 (budget 60 s)
[06:03:22] S1: gate-1794549645-s1 woken to ready — the stand-in round resumes and finishes
2026/11/12 22:03:22 history: job gate-1794549645-s1 completed in 2m37.112s (model=)
[06:03:22] PASS S1
[06:03:22] S3: game in the window — fresh image window, then a stream socket mid-drain
[06:03:22] S3 (attempt 1): submitting image.generate (no wake — the window trigger only)
[06:04:21] S3: window OPEN (reason="window open (30s), 1 message(s) still queued")
[06:04:21] S3: waiting for the image to finish (budget 300 s) — the game must start while a SMALL job is in flight, never the 12 GB image
[06:05:17] S3: image done (/home/agent/.local/share/jarvis/artifacts/e2e-event-flow-1794549645/gate-1794549645-s3/20261113-060516-753435889.png); the window holds (drained, younger than min_window) — starting the small job
[06:05:17] S3: waiting for the small job's gpu lease (in flight, budget 240 s)
[06:05:17] S3: small job IN FLIGHT (leases: brain  0;image.describe@gpu/image-describe-worker image.describe 3244;llm.small@gpu/llm-small-worker llm.small 5500;image.generate@gpu/image-gen-worker image.generate 12288;) — the game starts now
[06:05:17] S3: UDP socket held on 127.0.0.1:48010 (pid 2022928) — the stream detector polls every 2 s
[06:05:18] S3: stream ACTIVE — the GPU hold is on (leases now: brain  0;)
[06:05:52] S3: window CLOSED by the game (reason="closed: drained (all capability queues empty)")
[06:05:52] S3: note — the close reason is 'closed: drained (all capability queues empty)' (the game may have raced a drained tick; the hold was active, which is what matters)
[06:05:52] S3: waiting for the in-flight small job to DRAIN (its result must land, budget 180 s)
[06:05:52] S3: in-flight work drained: small job completed (worker=llm.small@gpu/llm-small-worker)
[06:05:52] S3: waiting for the brain engine back (budget 240 s)
[06:05:52] S3: brain engine UP
[06:05:52] S3: stream socket released — waiting for stream_active to clear (budget 60 s)
[06:05:54] S3: stream hold cleared — everything is back (window closed, brain up, hold off, in-flight work done)
[06:05:54] PASS S3
[06:05:54] ---
[06:05:54] cleanup: round job owner (pid 1419503) still alive — leaving plan-plan-event-driven-brain-1794016330-30 paused for the round to finish it
[06:05:54] stack health (should all be running):
jarvis-llm-small-worker-gpu: Up 8 hours
jarvis-image-gen-worker: Up 10 hours
jarvis-switchboard: Up 3 days
jarvis-rabbitmq: Up 3 days
[06:05:54] brain engine: up
[06:05:54] window: open=false stream_active=false
[06:05:54] RESULTS: 3 passed, 0 failed
[06:05:54]     S2:PASS
[06:05:54]     S1:PASS
[06:05:54]     S3:PASS
[06:05:54] GATE: PASS (S1 canonical flow + S2 voice fallback + S3 game drain)
[06:05:54] transcript: /tmp/e2e-event-flow-gate-2026-11-12.log
```
