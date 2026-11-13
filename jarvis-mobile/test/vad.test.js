// VAD state-machine tests — run with:  node test/vad.test.js
//
// src/vad.js is a plain ES module with no imports, so we load it from its
// source as a data: URL and drive tick() with scripted mic levels (the PWA
// fix used the same harness approach — no device, no recorder, just the
// decision logic).

const fs = require('fs');
const path = require('path');

const TICK = 100; // ms — same cadence the app polls metering at

let pass = 0;
let fail = 0;
function check(name, cond, detail) {
  if (cond) {
    pass++;
    console.log('ok   ' + name);
  } else {
    fail++;
    console.log('FAIL ' + name + (detail ? ' — ' + detail : ''));
  }
}

// Tick vad from t=TICK upward (100 ms steps) feeding levelAt(t) as the mic
// level. Returns {decision, at} of the first 'stop'/'cap', or null if none
// before tMax.
function run(vad, levelAt, tMax = 120000) {
  for (let t = TICK; t <= tMax; t += TICK) {
    const d = vad.tick(t, levelAt(t));
    if (d === 'stop' || d === 'cap') return { decision: d, at: t };
  }
  return null;
}

(async () => {
  const src = fs.readFileSync(path.join(__dirname, '..', 'src', 'vad.js'), 'utf8');
  const mod = await import('data:text/javascript,' + encodeURIComponent(src));
  const { createVad } = mod;

  // 1. Never speaks: silence is counted from t=0, so it auto-stops at 5 s
  //    (and no earlier, since min-record is 2 s < 5 s here).
  {
    const vad = createVad();
    vad.start(0);
    const r = run(vad, () => 0);
    check('never-speaks auto-stops at 5 s', r && r.decision === 'stop' && r.at === 5000,
      JSON.stringify(r));
  }

  // 2. Speaks until t=3 s, then silence → last speech at the t=2900 tick,
  //    stop at 2900 + 5000 = 7900.
  {
    const vad = createVad();
    vad.start(0);
    const r = run(vad, (t) => (t < 3000 ? 0.1 : 0));
    check('speech then 5 s silence stops at last-speech + 5 s',
      r && r.decision === 'stop' && r.at === 7900, JSON.stringify(r));
  }

  // 3. A 2.9 s pause (well under 5 s) between phrases must NOT cut the
  //    utterance; the countdown restarts and the stop lands 5 s after the
  //    second phrase ends.
  {
    const vad = createVad();
    vad.start(0);
    const levelAt = (t) =>
      (t < 2000 || (t >= 4900 && t < 7000)) ? 0.1 : 0;
    const r = run(vad, levelAt);
    check('sub-5 s pause does not cut the recording',
      r && r.decision === 'stop' && r.at === 11900, JSON.stringify(r));
  }

  // 4. Min-record: with silenceMs=500 the silence window expires at 600 ms,
  //    but the take may not stop before 2000 ms of recording.
  {
    const vad = createVad({ silenceMs: 500, minRecordMs: 2000 });
    vad.start(0);
    const r = run(vad, (t) => (t < 200 ? 0.1 : 0));
    check('never auto-stops before min-record',
      r && r.decision === 'stop' && r.at === 2000, JSON.stringify(r));
  }

  // 5. Continuous speech hits the 30 s safety cap, no matter how long the
  //    "sentence" goes on.
  {
    const vad = createVad();
    vad.start(0);
    const r = run(vad, () => 0.1);
    check('30 s safety cap on nonstop speech',
      r && r.decision === 'cap' && r.at === 30000, JSON.stringify(r));
  }

  // 6. Levels below VAD_THRESHOLD (0.01) count as silence: 0.005 for the
  //    whole take behaves like the never-speaks case → stop at 5 s.
  {
    const vad = createVad();
    vad.start(0);
    const r = run(vad, () => 0.005);
    check('sub-threshold level counts as silence',
      r && r.decision === 'stop' && r.at === 5000, JSON.stringify(r));
  }

  // 7. Tap-to-stop: after vad.stop() the machine is idle, regardless of
  //    levels (this is what the ⏹ button triggers).
  {
    const vad = createVad();
    vad.start(0);
    vad.tick(100, 0.1);
    vad.stop();
    check('stop() makes the machine idle', vad.tick(200, 0.1) === 'idle');
  }

  // 8. Before start(), ticks report idle.
  {
    const vad = createVad();
    check('ticks before start() are idle', vad.tick(500, 0.5) === 'idle');
  }

  console.log('\n' + pass + ' passed, ' + fail + ' failed');
  process.exit(fail ? 1 : 0);
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
