# Diagnosis: local runner "spin-up then die" + voice-chat mic error

Job: `chat-1793055012-13e6` (2026-10-26). Diagnostic only — no fixes made this round.

Owner's report (Discord 15:50 PDT): *"It reacted, made sounds, but then just died out."*
Owner's follow-up instruction for this round: mic error `Unsupported buffer size for ScriptProcessorNode`.

## 1. The mic error (root cause found — deterministic bug)

`scripts/voice_chat.html:480-481` (identical in deployed `/opt/jarvis-voice/scripts/voice_chat.html`):

```js
const bufferSize = SAMPLE_RATE * (CHUNK_DURATION_MS / 1000);   // 16000 * 0.2 = 3200
scriptProcessor = audioCtx.createScriptProcessor(bufferSize, 1, 1);
```

`createScriptProcessor()` requires `bufferSize` to be a **power of two in [256, 16384]**.
3200 is neither (2048 < 3200 < 4096), so browsers throw
`NotSupportedError: Unsupported buffer size for ScriptProcessorNode.` — exactly the text
Nathan's browser showed (caught at `:522` → `showMicError`).

Consequences, all confirmed:

- **Deterministic**: the browser mic-capture path in the voice-chat page has never worked.
  Every `startListening()` dies at line 481.
- **Silent server side**: the throw happens *before* `ws.send({type:'start'})` (`:517`), so the
  server never learns a session began. Journal matches: `23:04:34` and `23:09:41` show
  `WebSocket /voice/ws [accepted]` / `connection open` and then **zero** STT/LLM/TTS activity.
- Latent second bug (for the fix round): the page assumes the `AudioContext` runs at 16 kHz, but
  browsers typically run 44.1/48 kHz — even a power-of-two buffer would mislabel chunk duration.
  `ScriptProcessorNode` is also deprecated (AudioWorklet is the modern API).

A fix job is already queued for this: `chat-1793081477-012d` (from the 23:11 message).

## 2. The runner: what it is

"Local runner" = the local model stack: `llama-server` (27.3B Q4_K `jarvis-brain`,
ctx 131072, `--parallel 2`, 20.7 GB RAM) served by `jarvis-engine.service` on `:11434`.
Clients: `agent-run` rounds (Claude Code SDK → Anthropic `/v1/messages`), `jarvis-voice`
(OpenAI `/v1/chat/completions`, `model: jarvis-brain`), `jarvis-chat`.

Live-verified this round: the current server accepts `jarvis-brain` on **both** API paths
(it reports the model ID as the full gguf path, but falls back to the loaded model).
The `[claude-code:unrecognized_model] {"model":"jarvis-brain"}` line that appears in every
local round's transcript is **expected SDK telemetry** — documented at `scripts/agent-local:54` —
not a failure.

## 3. Timeline (all 2026-10-26, PDT; sources: sudo journal, systemd state, agent.log, voice journal)

- `00:34` — machine boots (same boot ever since); `jarvis-engine` enabled, comes up.
- `11:41–12:35` — benchmark round `chat-1792976974-392b` (qwen3.8/FreeToken VRAM sweep, run on
  Sonnet) runs. **`11:52:47` it executes `sudo systemctl stop jarvis-engine`** to free VRAM.
  A clean stop is *not* restarted, even with the `Restart=always` drop-in
  (`jarvis-engine.service.d/restart-always.conf`).
- `11:53–19:12` — the production endpoint is gone, but *something* kept serving `:11434`:
  voice LLM calls succeed at 15:17–15:20 (round 23's own live test) and 16:04 (from the HA box).
  Consistent with the benchmark round's **manual (non-systemd) `llama-server`** — its output
  went nowhere (no journal unit), which is why this window has no logs at all.
- **`15:50` — owner: "It reacted, made sounds, but then just died out."** No TTS/STT/LLM
  entries exist in the voice-service journal for ~15:20–16:04, so the sounds were
  **HA-side TTS** (VPE pipeline, `tts.piper` / google-translate — same pattern documented in
  `docs/phase4-voice-ha-log-capture.md`). The local brain was alive through this window
  (15:18 and 16:04 LLM calls 200 OK; coding rounds 9–22 all completed). Most plausible
  reading: an HA VPE session played a response and then went silent — the known dead
  `stt.speech_to_phrase` root cause (diagnosed 16:45–18:36, PRs #61/#62 territory), whose
  exact error text still needs `home-assistant.log` (no SSH to the HA box — open item).
  The chatbot misread the report as a local-runner crash and queued this job.
- `18:51:47` / `19:06:41` — `sudo pkill -f llama-server.*18080|18081` (benchmark cleanup).
- `19:12:33` — `sudo pkill -f llama-server|llama-bench` — this pattern matches **any**
  `llama-server`, so it killed whatever manual server was still holding `:11434`.
- `19:36–20:31` — **14 `agent-run` jobs fail**: `ECONNREFUSED` (nothing on `:11434` after the
  19:12 pkill) and `unrecognized model jarvis-brain` (clients hitting a non-brain server state).
  This is the "14x" in the 20:14 failure inventory.
- `19:58:56` — `jarvis-engine` active again (still active, `NRestarts=0`, healthy; started just
  before round 32 at 19:59:26, which ran fine on the local model). No `systemctl start`
  appears in the sudo journal — attribution of this start isn't captured (see gaps below).
- `21:37–21:52` — 5 more failures from the `agent-local --reasoning-effort` flag bug
  (since fixed: `scripts/agent-local:61-73` accepts both `--effort` and the old flag).
- OOM: **none**. `dmesg` clean for the day; 128 GB RAM; engine peak 20.7 GB. The single
  `signal:killed` failure was the 52-minute VRAM benchmark job (expected).

**Why it "stopped after reacting"**: the production brain was stopped at 11:52 for a VRAM
benchmark and never re-armed; voice/chat kept running on a manual benchmark server until the
19:12 pkill, after which every local-model client failed for ~46 minutes until the engine
returned at 19:58. The 15:50 "died out" the owner heard was the HA-side voice pipeline going
silent (dead STT), not a local-runner crash — but the two events share the same observability
problem: **none of the processes involved left logs of their death.**

## 4. Observability gaps found (why no crash output was capturable)

1. `systemd/jarvis-engine.service` runs llama-server with `--log-disable` → model-load and
   crash output is discarded by design; `journalctl -u jarvis-engine` is empty.
2. This machine's journal does not carry systemd unit lifecycle lines (no "Started/Stopped
   unit" entries for any unit) — start/stop attribution relies on the sudo audit lines.
3. Benchmark rounds launch manual `llama-server` instances whose output goes nowhere, and one
   `pkill` pattern (`llama-server|llama-bench`) is broad enough to kill the production engine.
4. `voice_service.py` does not log inbound text (known since the 17:12 note).

## 5. Recommendations (for later rounds — not acted on)

- Mic fix (queued as `chat-1793081477-012d`): power-of-two buffer (e.g. 2048), drive chunk
  math from `audioCtx.sampleRate`, consider AudioWorklet.
- Remove `--log-disable` from the engine service (or redirect to a file) so model-load errors
  and segfaults are captured.
- Benchmark jobs must not `stop` the production engine: use separate ports/instances (they
  already used 18080/18081) or `systemctl restart` after, and narrow any pkill pattern to
  their own ports/PIDs.
- Add inbound-text logging to `/voice/json` + WS sessions.
- Close the HA-box open item: read `home-assistant.log` (needs SSH/Supervisor) for the exact
  VPE/STT error text behind the 15:50 silence.

## Evidence index

- `notes/conversations/discord-2026-10-26.md` — owner messages 15:50 / 23:10 / 23:11 (verbatim).
- `journalctl` sudo audit: `systemctl stop jarvis-engine` (11:52:47), pkill ×3 (18:51–19:12).
- `systemctl status jarvis-engine` — active since 19:58:56, drop-in `restart-always.conf`.
- `~/.local/state/jarvis/agent.log` — round list; telemetry line in every local round.
- `jarvis-voice` journal — 23:04/23:09 WS opens with no audio; 15:18/16:04 LLM 200 OK.
- Live probes this round: `/v1/models`, `/v1/messages`, `/v1/chat/completions` with
  `model=jarvis-brain` (all accepted); `dmesg` (no OOM); `ps`/`ss` (llama-server pid 3389561).
