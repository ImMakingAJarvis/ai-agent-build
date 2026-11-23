// sim.js — the RAC track + car core (docs/sim-racetrack.md, RAC-2).
//
// Pure functions, no DOM: the page (page.js) and the headless harness
// (sim.test.js) both run exactly this code.
//
// Track shape: a stadium — two straights joined by two semicircular
// ends. The centerline is the set of points at distance r from the
// "spine" (the segment between the two arc centers, centered on the
// origin). The road is the band |d(p) - r| <= halfW around it, where
// d(p) is the distance from p to the spine. The page draws the road by
// stroking that same centerline at width 2*halfW, so the drawn road and
// the drivable band are the same region.
//
// Exactly two colors on the page: black background, white road. The car
// is a black rectangle on the road.

"use strict";

const Track = {
  a: 220,      // half the spine: distance from the center to each arc center
  r: 120,      // centerline radius (the arc radius)
  halfW: 52,   // half the road width
  canvasW: 960,
  canvasH: 600,

  // Closest point on the spine segment to (x, y).
  closestOnSpine(x, y) {
    return { x: Math.max(-this.a, Math.min(this.a, x)), y: 0 };
  },

  // Distance from (x, y) to the track centerline.
  distToCenterline(x, y) {
    const q = this.closestOnSpine(x, y);
    return Math.hypot(x - q.x, y - q.y);
  },

  // True when (x, y) is on the road, at least margin from either edge.
  onRoad(x, y, margin = 0) {
    return Math.abs(this.distToCenterline(x, y) - this.r) <= this.halfW - margin;
  },

  // Collision: clamp (x, y) into the road band, keeping the point at
  // least carRadius from both edges. Returns { x, y, hit } with hit one
  // of null | "outer" | "inner".
  clampToRoad(x, y, carRadius) {
    const q = this.closestOnSpine(x, y);
    const d = Math.hypot(x - q.x, y - q.y);
    const maxD = this.r + this.halfW - carRadius;
    const minD = this.r - this.halfW + carRadius;
    if (!(d > maxD || d < minD)) return { x, y, hit: null };
    if (d < 1e-9) { // unreachable in practice (minD > 0); guard the divide
      return { x: q.x, y: minD, hit: "inner" };
    }
    const t = (d > maxD ? maxD : minD) / d;
    return {
      x: q.x + (x - q.x) * t,
      y: q.y + (y - q.y) * t,
      hit: d > maxD ? "outer" : "inner",
    };
  },
};

const Car = {
  // Tuning (px, s, rad)
  maxSpeed: 220,
  maxReverse: 90,
  accel: 170,
  brake: 320,
  reverseAccel: 90,
  friction: 80,     // coasting decay
  steerRate: 2.6,   // rad/s at max speed; scales linearly with |speed|
  carRadius: 7,     // collision radius (half the car's long side)

  create(x, y, heading) {
    return { x, y, heading, speed: 0 };
  },

  // One physics step. input = { throttle, brake, steerLeft, steerRight },
  // each 0/1. Mutates and returns the state.
  step(s, input, dt) {
    if (input.throttle) {
      s.speed += this.accel * dt;
    } else if (input.brake) {
      s.speed -= (s.speed > 0 ? this.brake : this.reverseAccel) * dt;
    } else if (s.speed > 0) {
      s.speed = Math.max(0, s.speed - this.friction * dt);
    } else if (s.speed < 0) {
      s.speed = Math.min(0, s.speed + this.friction * dt);
    }
    s.speed = Math.max(-this.maxReverse, Math.min(this.maxSpeed, s.speed));

    // Steering scales with speed and is zero while stopped. In canvas
    // coordinates (y grows down), increasing heading turns the car
    // clockwise on screen, so steerLeft is negative.
    const steer = (input.steerRight ? 1 : 0) - (input.steerLeft ? 1 : 0);
    if (steer !== 0 && s.speed !== 0) {
      const rate = this.steerRate * (Math.abs(s.speed) / this.maxSpeed);
      s.heading += steer * rate * Math.sign(s.speed) * dt;
    }

    s.x += Math.cos(s.heading) * s.speed * dt;
    s.y += Math.sin(s.heading) * s.speed * dt;

    // Track edges: the car cannot leave the road. A hit kills most of
    // the speed and bounces the car slightly back.
    const c = Track.clampToRoad(s.x, s.y, this.carRadius);
    if (c.hit) {
      s.x = c.x;
      s.y = c.y;
      s.speed = -0.3 * s.speed;
    }
    return s;
  },
};

if (typeof module !== "undefined") module.exports = { Track, Car };
if (typeof globalThis !== "undefined") globalThis.Racetrack = { Track, Car };
