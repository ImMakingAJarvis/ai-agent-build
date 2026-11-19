# Voice call — the conversational VRAM takeover

Nathan (voice, 2026-11-18): talk to the system like a call — wake word,
then a trigger phrase ("talk to me"), and the conversational model takes
over the GPU for the whole session: short back-and-forth, ending on
"okay, thanks, bye" or 5 minutes of no one talking to it. While it is
listening it hears the room but only responds when addressed (loud
enough); background conversation does not interrupt it.

This doc is the design. The mechanics live in `scripts/voice_service.py`
(session state machine), `scripts/jarvis-swap` (`--voice`), and
`cmd/resourced` (`POST/DELETE /window/voice`); the runbook for a live
test is at the bottom.

## The pieces

- **Session owner** = the `jarvis-voice` container. It hears every
  utterance (satellite text turns, `/voice` audio, `/voice/ws` real-time
  audio), decides when a call starts/ends, and owns the idle timer.
- **Call engine** = a second brain: compose service `engine-voice`
  (container `jarvis-engine-voice`), the same 27B GGUF as the brain,
  fresh short context (`--parallel 1 --ctx-size 32768`), port 11436.
  Profile-gated (`profiles: ["voice"]`) so it never starts on its own —
  only `jarvis-swap out --voice` starts it.
- **The window** = a voice swap window, the same mechanism as the game
  window (resourced holds it open in steady state instead of the tick
  policy deciding). `jarvis-swap out --voice` stops the brain, starts
  engine-voice, waits for its health; `in` reverses. The voice service
  requests the window through resourced (`POST /window/voice`) and
  releases it (`DELETE /window/voice`); resourced's
  `AcquireForVoice`/`ReleaseFromVoice` run the swap under the same
  `swapMu` serialization the game path uses, and the 30s tick holds an
  open voice window steady (like game).
- **Failsafe** = the watchdog maintenance window, opened BEFORE the
  brain stops (maintain 2h). A wedged voice session, a dead
  jarvis-voice, or a crashed resourced can never strand the brain: at
  the deadline the watchdog re-arms it. The session's own max length
  (60 min) is well inside the maintain.

## Session lifecycle

States in `voice_service.py` (single session at a time):

```
idle ──trigger phrase──▶ opening ──window open + engine-voice healthy──▶ active
 ▲                                                                  │
 └────────────── goodbye phrase / idle 5 min / max 60 min ◀──────────┘
```

- **Trigger** (starts a call, matched on the transcript): "talk to me",
  "let's talk", "let's chat", "start a conversation", "start a call",
  "chat with me", "I want to talk", "have a conversation". The trigger
  turn itself gets a canned reply ("On it — spinning up my conversation
  brain, give me a couple of minutes"), no LLM call: the window takes
  1–3 min (the satellite's 120 s timeout would otherwise time out the
  turn).
- **While `opening`**: normal turns still work, on the normal path
  (brain if it still answers — `out` may not have completed yet — else
  the existing iGPU fallback). No second window request.
- **While `active`**: turns go to engine-voice (`http://engine-voice:
  11436`, model `jarvis-brain`) with a call-specific system prompt
  (short, conversational, 1–3 sentences) and a SEPARATE session history
  (the call does not pollute the normal voice history and vice versa).
  If engine-voice is down mid-call, the turn degrades to the normal
  `resolve_llm` path (brain/fallback) — the call keeps working, just
  slower, and `/voice/session` shows `engine: down`.
- **End**: a goodbye phrase ("bye", "goodbye", "see you", "that's all",
  "thanks, bye", "good night", "talk later", …) → a short canned
  goodbye, release the window, back to `idle`. Or 5 min
  (`VOICE_SESSION_IDLE_S=300`) with no *addressed* speech — the idle
  watchdog checks every 30 s. Or 60 min total (`VOICE_SESSION_MAX_S`).
- **Degraded mode**: if resourced is unreachable when a trigger lands,
  the session still starts (turns use the normal path — the brain is
  still up, no VRAM takeover). `/voice/session` says
  `window: not requested (resourced unreachable)`; the next trigger or
  watchdog tick retries.

## Addressed gating (the "is it for me" check)

The call hears everything the mics pick up. An utterance counts as
*addressed* — gets a reply and resets the idle timer — only if it is
loud enough:

- **Audio paths** (`POST /voice`, `/voice/ws`): RMS of the utterance
  buffer in dBFS. Below `VOICE_LEVEL_MIN_DB` (default −40) → the turn
  is ignored: no reply, no history, no timer reset. `/voice` answers
  `{"status": "ignored", "reason": "not addressed"}` (JSON, not audio);
  the WS sends a `{"type":"status","status":"ignored"}` frame.
  Normal speech at normal distance is roughly −30…−18 dBFS;
  background conversation at distance is below −40. The threshold is
  env-tunable per room (`VOICE_LEVEL_MIN_DB`).
- **Text path** (HA satellite, `/voice/json`): no audio level exists,
  but every satellite turn is already wake-gated ("okay jarvis, …") —
  background conversation never produces a satellite turn. So a text
  turn is always addressed.
- Trigger and goodbye phrases bypass the level check (they define the
  session's edges; a quiet "bye" should still end the call).

## The VRAM window

- `jarvis-swap out --voice --reason <r> --force`: like the game window
  (NO photo services — voice does not generate photos), plus:
  `docker compose --profile voice up -d engine-voice` + wait for
  `127.0.0.1:11436/health` (15 min, 27B Q4 load). The round guard
  counts only **driver-local** running jobs (see below), so a voice
  window can open while an igpu round is running.
- `jarvis-swap in`: stops `jarvis-engine-voice` (with art/bench) before
  starting the brain.
- resourced: `POST /window/voice` → `AcquireForVoice` (idempotent
  steady-state no-op if already open for voice) → `out --voice`;
  `DELETE /window/voice` → `ReleaseFromVoice` (only closes windows
  whose `for` list contains "voice"). The 30s tick holds an open voice
  window exactly like game — the queue policy never closes a window the
  voice service owns.
- **Round guard narrowing** (this PR): the guard previously counted
  *any* running job as "a round is using the brain". That is only true
  for `driver: local` jobs — igpu rounds run on the iGPU and are
  unaffected by a brain stop. The guard now reads each running job's
  driver (`jarvis-jobs show`) and counts local ones. Invariant 1
  (never stop the brain mid-round) is unchanged for brain rounds; it
  lets voice/test windows open while igpu work runs.

## Failure modes — all bounded, none lose the brain

- **engine-voice fails to load** (27B won't fit, corrupt GGUF): `out`
  times out after the health wait → the window is not reported open;
  the voice service sees `window: requested, not open`, keeps the
  session on the degraded path, and retries on the next trigger. The
  maintain deadline re-arms the brain either way.
- **The voice service dies mid-call**: the session and its timer die
  with it; the window stays open until the maintain deadline (2h)
  re-arms the brain. Nothing polls forever.
- **resourced dies mid-call**: `AcquireForVoice`/`ReleaseFromVoice`
  are the only window drivers for voice; if resourced is down, the
  voice service retries on each trigger and the session degrades (no
  takeover). The maintain deadline still bounds the brain-down time
  from the last successful `out`.
- **A local job is queued mid-window**: `jarvis-jobs next` skips it
  (brain down); it runs after the window. Same as photo/game.
- **The call runs long (60 min max)**: the session ends itself,
  releases the window, `in` re-arms the brain — 2h of maintain have
  passed, so nothing else could have.
- **Trigger while a game window is open** (or vice versa): the swap
  is serialized by resourced's `swapMu`; the second requester finds
  the window open *for a different purpose*, reports it as not
  available, and its session degrades (normal path) — no double-stop,
  no window stealing.

## Endpoints

- `GET /voice/session` — `{state, window, turns, running_s?, idle_s?,
  window_error?, engine_up}`. Used by the test and the dashboard.
- `POST /voice/session/force-end` — kill switch: end the session and
  release the window, no goodbye phrase needed.

## Test run (the "maybe we could do a test run")

`scripts/voice_session_test.py` runs inside the voice container
(`docker exec jarvis-voice python3 /opt/jarvis-voice/scripts/voice_
session_test.py`) — it has edge-tts, httpx, numpy, soundfile, reaches
the service on 127.0.0.1:8100 and the engines by compose name
(`engine:11434`, `engine-voice:11436`). It needs **no round running**
(it stops the brain), so it is queued as a `-driver igpu` job whose
instructions are: run the script, read its PASS/FAIL, notify Discord,
finish 0/1.

Steps: clean slate (force-end a call left open by a dead previous run)
→ synthesize "Okay, talk to me." → `POST /voice` (expect the canned
opening, `state=opening`) → poll `/voice/session` until
`state=active` + `window=open` (≤6 min: window + engine load) → a
normal turn (expect WAV + `X-Reply`) → a quiet background turn at
~−45 dBFS, below the −40 gate (expect `{"status": "ignored"}`) →
"Okay, thanks, bye." (expect the canned goodbye, `state=idle`) → poll
until `window=closed` + brain healthy on `engine:11434` (≤6 min) →
PASS/FAIL on the last line, exit 0/1, each step logged with
timestamps.

A **live** run by Nathan: say "okay jarvis, talk to me" to a satellite,
wait for the "spinning up" reply (a couple of minutes), then just talk.
"Okay, thanks, bye" ends it. `jarvis-swap status` and
`/voice/session` show the whole time what is happening.
