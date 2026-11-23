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
    getContext() {
      return new Proxy({}, {
        get(_, prop) { return (..._args) => { calls.push(prop); }; },
        set(_, prop, v) { calls.push("set:" + prop); return true; },
      });
    },
  };
}
const ctxCalls = [];       // the #track canvas
const camCtxCalls = [];    // the #cam (CAM panel) canvas
const canvas = fakeCanvas(ctxCalls);
const camCanvas = fakeCanvas(camCtxCalls);

const listeners = {};
globalThis.window = {
  addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
};
const byId = { track: canvas, cam: camCanvas };
globalThis.document = {
  getElementById(id) {
    assert.ok(byId[id], "page only asks for #track and #cam (got #" + id + ")");
    return byId[id];
  },
};
let rafCb = null;
globalThis.requestAnimationFrame = (fn) => { rafCb = fn; return 1; };

// Load order matches index.html: sim.js sets globalThis.Racetrack, camera.js
// adds .Camera, page.js wires the DOM and reads both.
require("./sim.js");
require("./camera.js");
require("./page.js");

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

console.log("page.test: PASS — key handlers drive the car, draw calls fire, road kept");
