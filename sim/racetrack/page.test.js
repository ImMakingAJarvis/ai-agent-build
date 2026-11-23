// page.test.js — drives the real page.js with a fake DOM (no browser on
// this box): `node sim/racetrack/page.test.js`. Verifies the page wiring
// that sim.test.js cannot: the key listeners feed the car's input, the
// rAF loop steps the physics, and the draw calls happen. The pixels
// themselves still need a human browser (see the PR).

"use strict";

const assert = require("assert");

// --- fake DOM ----------------------------------------------------------------
const ctxCalls = [];
const canvas = {
  width: 0,
  height: 0,
  getContext() {
    return new Proxy({}, {
      get(_, prop) { return (..._args) => { ctxCalls.push(prop); }; },
      set(_, prop, v) { ctxCalls.push("set:" + prop); return true; },
    });
  },
};

const listeners = {};
globalThis.window = {
  addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
};
globalThis.document = {
  getElementById(id) {
    assert.strictEqual(id, "track", "page asks for the #track canvas");
    return canvas;
  },
};
let rafCb = null;
globalThis.requestAnimationFrame = (fn) => { rafCb = fn; return 1; };

require("./sim.js");  // first, as in index.html: it sets globalThis.Racetrack
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

// Non-arrow keys are ignored (no crash, no input change).
listeners.keydown[0]({ key: "a", preventDefault() {} });
assert.strictEqual(page.input.throttle, 0, "non-arrow keys do not drive");

console.log("page.test: PASS — key handlers drive the car, draw calls fire, road kept");
