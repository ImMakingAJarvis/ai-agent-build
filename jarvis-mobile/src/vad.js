// Voice-activity detection — ported from the PWA fix (PR #79, commit
// c0175fb in scripts/jarvis_app/app.js). The PWA watched the live mic with a
// Web Audio AnalyserNode (RMS of the time-domain buffer); the native app
// watches the recorder's metering level (expo-audio `getStatus().metering`)
// instead, but the decision logic is the same:
//
//   - any speech-level sample resets the silence countdown, so pauses
//     shorter than VAD_SILENCE_MS never cut an utterance
//   - after VAD_SILENCE_MS without speech the recording stops and is sent
//   - the recording is never stopped before VAD_MIN_RECORD_MS
//   - a MAX_RECORD_MS safety cap stops an utterance that never goes quiet
//
// The pure tick() below makes the whole state machine unit-testable without
// a device (test/vad.test.js drives it with scripted levels).

// Auto-stop tuning — adjust here, no digging needed:
//   VAD_SILENCE_MS   — stop and send after this long without speech
//   VAD_MIN_RECORD_MS — never auto-stop before this much recording
//   VAD_THRESHOLD    — level (0..1) at or above this counts as speech; raise
//                      it for noisy rooms, lower it for quiet speakers
//   MAX_RECORD_MS    — hard stop no matter what (same 30 s cap as the PWA)
export const VAD_SILENCE_MS = 5000;
export const VAD_MIN_RECORD_MS = 2000;
export const VAD_THRESHOLD = 0.01;
export const MAX_RECORD_MS = 30000;
export const VAD_TICK_MS = 100;

export function createVad(opts = {}) {
  const silenceMs = opts.silenceMs ?? VAD_SILENCE_MS;
  const minRecordMs = opts.minRecordMs ?? VAD_MIN_RECORD_MS;
  const threshold = opts.threshold ?? VAD_THRESHOLD;
  const maxMs = opts.maxMs ?? MAX_RECORD_MS;

  let running = false;
  let startedAt = 0;
  let lastSpeechAt = 0;

  return {
    get isRunning() {
      return running;
    },
    start(now) {
      startedAt = now;
      // Same as the PWA: silence is counted from the moment recording
      // starts, so a take that never hears speech auto-stops too.
      lastSpeechAt = now;
      running = true;
    },
    // Returns the decision for this tick:
    //   'speaking'  — speech-level level seen, countdown reset
    //   'listening' — silence so far, keep recording
    //   'stop'      — silence expired (or the never-speaks case)
    //   'cap'       — max recording length reached
    //   'idle'      — not running
    tick(now, level) {
      if (!running) return 'idle';
      const speaking = typeof level === 'number' && level >= threshold;
      if (speaking) lastSpeechAt = now;
      // The cap is checked on every tick, speech or not — a 30 s nonstop
      // utterance must still be cut (same as the PWA).
      if (now - startedAt >= maxMs) return 'cap';
      if (
        now - lastSpeechAt >= silenceMs &&
        now - startedAt >= minRecordMs
      ) {
        return 'stop';
      }
      return speaking ? 'speaking' : 'listening';
    },
    stop() {
      running = false;
    },
  };
}
