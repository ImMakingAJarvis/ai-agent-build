// page.test.js — drives the real page.js with a fake DOM (no browser on
// this box): `node sim/racetrack/page.test.js`. Verifies the page wiring
// that sim.test.js cannot: the key listeners feed the car's input, the
// rAF loop steps the physics, and the draw calls happen. The pixels
// themselves still need a human browser (see the PR).

"use strict";

const assert = require("assert");

// --- fake DOM ----------------------------------------------------------------
function fakeCanvas(calls) {
  return {
    width: 0,
    height: 0,
    listeners: {},
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    click(x, y) {
      (this.listeners.click || []).forEach((fn) =>
        fn({ offsetX: x, offsetY: y, preventDefault() {} }));
    },
    getContext() {
      return new Proxy({}, {
        get(_, prop) { return (..._args) => { calls.push(prop); }; },
        set(_, prop, v) { calls.push("set:" + prop); return true; },
      });
    },
  };
}
function fakeButton(id, extra) {
  const b = Object.assign({
    id,
    className: "",
    listeners: {},
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    click() { (this.listeners.click || []).forEach((fn) => fn({ preventDefault() {} })); },
  }, extra || {});
  return b;
}
const ctxCalls = [];       // the #track canvas
const camCtxCalls = [];    // the #cam (CAM panel) canvas
const circCtxCalls = [];   // the #circuit (editor) canvas
const canvas = fakeCanvas(ctxCalls);
const camCanvas = fakeCanvas(camCtxCalls);
const circuitCanvas = fakeCanvas(circCtxCalls);
const gateButtons = ["NOT", "AND", "OR", "NAND", "NOR", "XOR", "XNOR"]
  .map((t) => fakeButton("gate-" + t, { dataset: { gate: t } }));
const byId = {
  track: canvas,
  cam: camCanvas,
  circuit: circuitCanvas,
  "circuit-json": { value: "" },
  "editor-delete": fakeButton("editor-delete"),
  "mode-manual": fakeButton("mode-manual"),
  "mode-auto": fakeButton("mode-auto"),
  "sim-reset": fakeButton("sim-reset"),
  "circuit-save": fakeButton("circuit-save"),
  "circuit-load": fakeButton("circuit-load"),
};
const listeners = {};
globalThis.window = {
  addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
};
globalThis.document = {
  getElementById(id) {
    assert.ok(byId[id], "unknown element #" + id);
    return byId[id];
  },
  querySelectorAll(sel) {
    assert.strictEqual(sel, ".gate-tool", "editor only queries .gate-tool");
    return gateButtons;
  },
};
let rafCb = null;
globalThis.requestAnimationFrame = (fn) => { rafCb = fn; return 1; };

// Load order matches index.html: sim.js sets globalThis.Racetrack, camera.js
// adds .Camera, gates.js adds .Gates, circuit.js adds .Circuit, page.js
// wires the DOM, editor.js wires the editor panel.
require("./sim.js");
require("./camera.js");
require("./gates.js");
require("./circuit.js");
require("./page.js");
require("./editor.js");

assert.ok(listeners.keydown && listeners.keydown.length === 1,
  "page registered one keydown listener");
assert.ok(listeners.keyup && listeners.keyup.length === 1,
  "page registered one keyup listener");
const page = globalThis.window.__racPage;
assert.ok(page && page.car, "page exposes its car state");

// The initial frame already drew the road (stroke) and the car (fillRect).
assert.ok(ctxCalls.includes("stroke"), "initial draw stroked the road");
assert.ok(ctxCalls.includes("fillRect"), "initial draw filled the car rectangle");

// --- the fake camera (RAC-3) -------------------------------------------------
assert.ok(page.camera, "page exposes the camera");
assert.strictEqual(globalThis.window.__racCamera, page.camera,
  "window.__racCamera is the page's camera");
assert.strictEqual(page.camera.cols, 16, "camera is 16 cols wide");
assert.strictEqual(page.camera.rows, 8, "camera is 8 rows deep");
assert.strictEqual(camCanvas.width, 16 * 16, "CAM panel sized to the grid width");
assert.strictEqual(camCanvas.height, 8 * 16, "CAM panel sized to the grid height");
assert.ok(camCtxCalls.includes("fillRect"), "initial draw painted the CAM panel");
const cam0 = page.camera.grid();
assert.ok(Array.isArray(cam0) && cam0.length === 8 && cam0[0].length === 16,
  "camera.grid() returns an 8x16 array");
assert.strictEqual(page.camera.pixel(0, 0), cam0[0][0], "pixel(r,c) matches grid()");
for (const row of cam0)
  for (const v of row) assert.ok(v === 0 || v === 1, "camera pixels are 0 or 1");

// --- drive it with the real key handlers -------------------------------------
const { Track, Car } = require("./sim.js");

let t = performance.now();
function press(key) { listeners.keydown[0]({ key, preventDefault() {} }); }
function release(key) { listeners.keyup[0]({ key, preventDefault() {} }); }
function tick() {
  assert.ok(typeof rafCb === "function", "the rAF loop reschedules itself");
  const cb = rafCb;
  rafCb = null;
  t += 16.7;
  cb(t);
}

assert.strictEqual(page.car.speed, 0, "the car starts stopped");
press("ArrowUp");
for (let i = 0; i < 90; i++) tick(); // 1.5s of throttle
assert.ok(page.car.speed > 150,
  `ArrowUp drives the car up to speed (speed=${page.car.speed.toFixed(1)})`);

const heading0 = page.car.heading;
press("ArrowRight");
for (let i = 0; i < 45; i++) tick(); // 0.75s of steering
assert.ok(Math.abs(page.car.heading - heading0) > 0.2,
  "ArrowRight turns the car while it is moving");

release("ArrowRight");
release("ArrowUp");
for (let i = 0; i < 240; i++) tick(); // coast to a stop
assert.strictEqual(page.car.speed, 0, "the car coasts back to a stop");

// Nothing may have left the road along the way.
const off = Math.abs(Track.distToCenterline(page.car.x, page.car.y) - Track.r)
  - Track.halfW;
assert.ok(off <= 1e-6, "the car is still on the road");

// The camera followed the car: the top-down road stays put, but the car's
// dot moved, so the grid is not the initial one (the CAM panel updates
// live as the car drives).
assert.notDeepStrictEqual(cam0, page.camera.grid(),
  "the camera grid changed as the car drove and turned");

// Non-arrow keys are ignored (no crash, no input change).
listeners.keydown[0]({ key: "a", preventDefault() {} });
assert.strictEqual(page.input.throttle, 0, "non-arrow keys do not drive");

// --- RAC-5: the circuit editor and AUTO mode ------------------------------
const editor = globalThis.window.__racEditor;
assert.ok(editor, "the editor is wired to the page");
assert.strictEqual(editor.mode, "MANUAL", "the page starts in MANUAL mode");
assert.ok(circCtxCalls.includes("fillRect"), "the editor drew the input block");
assert.ok(circCtxCalls.includes("stroke"), "the editor stroked wires/borders");

// Default circuit: all outputs const.0 -> AUTO is a parked car.
editor.setMode("AUTO");
for (let i = 0; i < 60; i++) tick();
assert.strictEqual(page.car.speed, 0, "AUTO with the empty circuit does not drive");

// Acceptance circuit: one front road pixel -> throttle. Back on the start
// line first: the car above ended wherever the MANUAL driving left it.
// The car starts at (220,120) facing -x; the camera frames the track bbox
// grown by 8px, so col 8 samples x = -400 + 8*(800/15) ~ 26.7, row 6
// samples y = -180 + 6*(360/7) ~ 128.6 — on the bottom straight, ahead
// of the car.
page.reset();
assert.strictEqual(page.camera.pixel(6, 8), 1, "the pixel ahead of the car is road");
assert.ok(editor.load(JSON.stringify({
  gates: {},
  outputs: { throttle: "cam.r6.c8", steerLeft: "const.0", steerRight: "const.0" },
})), "the acceptance circuit loads");
tick(); // the next frame evaluates the new circuit
assert.strictEqual(page.input.throttle, 1, "AUTO: throttle is on while the pixel sees road");
for (let i = 0; i < 30; i++) tick(); // 0.5s
assert.ok(page.car.speed > 60,
  `the car moves while that pixel sees road (speed=${page.car.speed.toFixed(1)})`);

// Park the car's dot on that pixel: it reads 0 -> the throttle releases.
page.car.x = 26.7; page.car.y = 128.6; page.car.heading = Math.PI;
assert.strictEqual(page.camera.pixel(6, 8), 0, "the pixel reads 0 under the car's dot");
tick();
assert.strictEqual(page.input.throttle, 0, "AUTO releases the throttle when the pixel sees the car");
assert.ok(page.car.speed < 88, "the car slows as soon as the pixel is no longer road");

// Back to MANUAL: the arrows drive again (and a stale AUTO value is zeroed).
editor.setMode("MANUAL");
assert.strictEqual(page.input.throttle, 0, "MANUAL zeroes the input on switch");
press("ArrowUp");
for (let i = 0; i < 90; i++) tick();
assert.ok(page.car.speed > 60, "MANUAL: arrow keys drive the car again");
release("ArrowUp");
for (let i = 0; i < 300; i++) tick(); // coast to a stop

// Place + wire through the editor's own click path (pixel -> gate -> output).
gateButtons[1].click(); // arm AND
circuitCanvas.click(450, 150); // place it in the gates area (lands at 406,128)
assert.ok(editor.circuit.gates.g1 && editor.circuit.gates.g1.type === "AND",
  "clicking the gates area places the armed gate");
circuitCanvas.click(16 + 2 * 18 + 9, 48 + 0 * 18 + 9); // arm wire from cam.r0.c2
circuitCanvas.click(406 - 5, 128 + 14); // -> g1 input pin 0
assert.strictEqual(editor.circuit.gates.g1.in[0], "cam.r0.c2",
  "pixel -> gate input wiring via clicks");
circuitCanvas.click(748, 48 + 48); // arm output throttle
circuitCanvas.click(406 + 80, 128 + 22); // <- g1 body (its output)
assert.strictEqual(editor.circuit.outputs.throttle, "g1",
  "gate -> output wiring via clicks");

// SAVE / LOAD round-trip through the textarea.
byId["circuit-save"].click();
const saved = JSON.parse(byId["circuit-json"].value);
assert.strictEqual(saved.outputs.throttle, "g1", "SAVE writes the circuit JSON");
const g1 = editor.circuit.gates.g1;
editor.circuit.gates.g1.in[0] = "const.1"; // tamper with the live circuit
byId["circuit-load"].click();
assert.strictEqual(editor.circuit.gates.g1.in[0], saved.gates.g1.in[0],
  "LOAD restores the circuit from the textarea");
assert.strictEqual(editor.circuit.gates.g1.in[0], "cam.r0.c2");
// LOAD rejects broken JSON (keeps the running circuit, reports the error).
byId["circuit-json"].value = "{not json";
byId["circuit-load"].click();
assert.strictEqual(editor.circuit.gates.g1.in[0], "cam.r0.c2",
  "a refused LOAD leaves the running circuit untouched");

// Cycles are refused by the model the editor uses.
assert.throws(() => {
  const C = require("./circuit.js");
  C.setGateInput(editor.circuit, "g1", 1, "g1");
}, /cycle|itself/);

// RESET: back on the start line, stopped.
byId["sim-reset"].click();
assert.strictEqual(page.car.x, 220, "RESET puts the car back on the start line");
assert.strictEqual(page.car.speed, 0, "RESET stops the car");

console.log("page.test: PASS — key handlers, camera panel, editor wiring, AUTO mode, save/load");
