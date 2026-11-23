// sim.test.js — headless physics checks for the RAC core (RAC-2).
//
// Runs the real sim.js in node (no DOM): `node sim/racetrack/sim.test.js`.
// Covers: track math, throttle/brake/reverse, friction, no turning while
// stopped, wall collision (the car cannot leave the road), and a full
// lap driven by a small pure-pursuit autopilot.

"use strict";

const assert = require("assert");
const { Track, Car } = require("./sim.js");

// --- track math sanity ----------------------------------------------------
assert.ok(Math.abs(Track.distToCenterline(Track.a, Track.r) - Track.r) < 1e-9,
  "start point is on the centerline");
assert.strictEqual(Track.distToCenterline(0, 0), 0, "spine midpoint is d=0");
assert.ok(Track.onRoad(Track.a, Track.r), "centerline is on the road");
assert.ok(Track.onRoad(Track.a, Track.r, Car.carRadius),
  "the car starts on the road, clear of the walls");
assert.ok(!Track.onRoad(Track.a, Track.r + Track.halfW + 20),
  "outside the outer edge is off-road");
assert.ok(!Track.onRoad(0, 40), "the infield is off-road");
console.log("ok track math");

// --- throttle, friction, reverse ------------------------------------------
let s = Car.create(Track.a, Track.r, Math.PI);
Car.step(s, { throttle: 1, brake: 0, steerLeft: 0, steerRight: 0 }, 2);
assert.strictEqual(s.speed, Car.maxSpeed, "throttle runs up to max speed");
for (let i = 0; i < 300; i++)
  Car.step(s, { throttle: 0, brake: 0, steerLeft: 0, steerRight: 0 }, 1 / 60);
assert.strictEqual(s.speed, 0, "coasting decays to a stop (friction)");
for (let i = 0; i < 300; i++)
  Car.step(s, { throttle: 0, brake: 1, steerLeft: 0, steerRight: 0 }, 1 / 60);
assert.ok(s.speed < 0, "brake drives into reverse");
assert.ok(s.speed >= -Car.maxReverse - 1e-9, "reverse is capped");
console.log("ok throttle / friction / reverse");

// --- no turning while stopped ----------------------------------------------
s = Car.create(Track.a, Track.r, Math.PI);
const h0 = s.heading;
for (let i = 0; i < 120; i++)
  Car.step(s, { throttle: 0, brake: 0, steerLeft: 1, steerRight: 0 }, 1 / 60);
assert.strictEqual(s.heading, h0, "no turning while stopped");
assert.strictEqual(s.x, Track.a, "the car did not move while stopped");
console.log("ok no turning while stopped");

// --- steering turns a moving car -------------------------------------------
s = Car.create(Track.a, Track.r, Math.PI);
for (let i = 0; i < 30; i++)
  Car.step(s, { throttle: 1, brake: 0, steerLeft: 1, steerRight: 0 }, 1 / 60);
assert.ok(s.heading < h0 - 0.2, "steer left turns the nose left while moving");
console.log("ok steering scales with speed");

// --- wall collision: the car cannot leave the road -------------------------
s = Car.create(Track.a, Track.r, Math.PI / 2); // centerline, facing the outer wall
let hitWall = false;
for (let i = 0; i < 240; i++) {
  Car.step(s, { throttle: 1, brake: 0, steerLeft: 0, steerRight: 0 }, 1 / 60);
  const off = Math.abs(Track.distToCenterline(s.x, s.y) - Track.r) - Track.halfW;
  assert.ok(off <= 1e-6, "car left the road while driving into the wall");
  if (s.speed <= 0) { hitWall = true; break; }
}
assert.ok(hitWall, "the wall stopped the car and bounced it back");
console.log("ok wall collision keeps the car on the road");

// --- autopilot: a full lap without leaving the road ------------------------
// Pure pursuit with a look-ahead: steer toward a point 40px ahead on the
// centerline. Aiming at a finite point (not the local tangent) pulls the
// car back to the centerline if it drifts to either edge of the road.
function autopilot(car) {
  const q = Track.closestOnSpine(car.x, car.y);
  const d = Math.hypot(car.x - q.x, car.y - q.y) || 1e-9;
  // closest point on the centerline: radially out from the spine point
  const px = q.x + (car.x - q.x) / d * Track.r;
  const py = q.y + (car.y - q.y) / d * Track.r;
  // local tangent at that centerline point
  let tx, ty;
  if (Math.abs(q.x) < Track.a) {
    // on a straight: the bottom straight (car below the spine) runs -x,
    // the top straight runs +x
    tx = car.y >= 0 ? -1 : 1;
    ty = 0;
  } else {
    // on an arc: tangent is perpendicular to the radial direction,
    // pointed along the current motion
    const cx = q.x < 0 ? -Track.a : Track.a;
    tx = -py;
    ty = px - cx;
    const L = Math.hypot(tx, ty) || 1;
    tx /= L; ty /= L;
    const vx = Math.cos(car.heading) * car.speed;
    const vy = Math.sin(car.heading) * car.speed;
    if (tx * vx + ty * vy < 0) { tx = -tx; ty = -ty; }
  }
  const lookX = px + tx * 40;
  const lookY = py + ty * 40;
  let diff = Math.atan2(lookY - car.y, lookX - car.x) - car.heading;
  while (diff > Math.PI) diff -= 2 * Math.PI;
  while (diff < -Math.PI) diff += 2 * Math.PI;
  const a = Math.abs(diff);
  return {
    throttle: a < 0.5 ? 1 : a < 0.9 ? 0.4 : 0, // brake into the corners
    brake: 0,
    steerLeft: diff < -0.03 ? 1 : 0,
    steerRight: diff > 0.03 ? 1 : 0,
  };
}

const car = Car.create(Track.a, Track.r, Math.PI);
let theta = Math.atan2(car.y, car.x);
let acc = 0;
let lastT = 0;
const dt = 1 / 240;
for (let t = 0; t < 120; t += dt) {
  Car.step(car, autopilot(car), dt);
  const off = Math.abs(Track.distToCenterline(car.x, car.y) - Track.r) - Track.halfW;
  assert.ok(off <= 1e-6, `left the road at t=${t.toFixed(2)}s (off=${off.toFixed(3)})`);
  const th = Math.atan2(car.y, car.x);
  let dth = th - theta;
  while (dth > Math.PI) dth -= 2 * Math.PI;
  while (dth < -Math.PI) dth += 2 * Math.PI;
  acc += dth;
  theta = th;
  lastT = t;
  if (Math.abs(acc) >= 2 * Math.PI) break;
}
assert.ok(Math.abs(acc) >= 2 * Math.PI - 0.05,
  `autopilot completed a full lap (accumulated ${acc.toFixed(2)} rad in ${lastT.toFixed(1)}s)`);
console.log(`ok full lap in ${lastT.toFixed(1)}s, never off the road`);

console.log("sim.test: PASS");
