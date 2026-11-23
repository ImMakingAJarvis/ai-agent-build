// page.js — the browser layer of the RAC page: canvas, arrow keys and
// the requestAnimationFrame loop. All physics lives in sim.js (pure);
// this file is the thin DOM wiring on top of it.
//
// Input: ArrowUp = throttle, ArrowDown = brake/reverse,
//        ArrowLeft / ArrowRight = steer.

"use strict";

function makePage(canvas) {
  const { Track, Car } = globalThis.Racetrack;
  const ctx = canvas.getContext("2d");
  const W = Track.canvasW;
  const H = Track.canvasH;
  canvas.width = W;
  canvas.height = H;

  // The car starts on the bottom straight, on the centerline, facing -x
  // (canvas y grows down, so the bottom of the track is +y).
  const car = Car.create(Track.a, Track.r, Math.PI);
  const keys = new Set();
  const input = { throttle: 0, brake: 0, steerLeft: 0, steerRight: 0 };

  function readKeys() {
    input.throttle = keys.has("ArrowUp") ? 1 : 0;
    input.brake = keys.has("ArrowDown") ? 1 : 0;
    input.steerLeft = keys.has("ArrowLeft") ? 1 : 0;
    input.steerRight = keys.has("ArrowRight") ? 1 : 0;
  }

  // Exactly two colors: #000 background, #fff road; the car is #000.
  function draw() {
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, W, H);

    ctx.save();
    ctx.translate(W / 2, H / 2);

    // The road: stroke the centerline (stadium) at width 2*halfW.
    ctx.strokeStyle = "#fff";
    ctx.lineWidth = 2 * Track.halfW;
    ctx.beginPath();
    ctx.moveTo(-Track.a, -Track.r);
    ctx.lineTo(Track.a, -Track.r);
    ctx.arc(Track.a, 0, Track.r, -Math.PI / 2, Math.PI / 2);
    ctx.lineTo(-Track.a, Track.r);
    ctx.arc(-Track.a, 0, Track.r, Math.PI / 2, Math.PI * 1.5);
    ctx.closePath();
    ctx.stroke();

    // The car: a small black rectangle with a heading.
    ctx.save();
    ctx.translate(car.x, car.y);
    ctx.rotate(car.heading);
    ctx.fillStyle = "#000";
    ctx.fillRect(-Car.carRadius, -Car.carRadius * 0.65,
                 Car.carRadius * 2, Car.carRadius * 1.3);
    ctx.restore();

    // Hint + speed readout: white on the black background.
    ctx.fillStyle = "#fff";
    ctx.font = "14px monospace";
    ctx.fillText("arrow keys: drive the car around the track", 16, 24);
    ctx.fillText("speed " + Math.round(car.speed) + " px/s", 16, 44);
    ctx.restore();
  }

  function frame(dt) {
    Car.step(car, input, dt);
    draw();
  }

  draw(); // paint the first frame immediately

  return {
    car,
    input,
    frame,
    draw,
    keydown(code) { keys.add(code); readKeys(); },
    keyup(code) { keys.delete(code); readKeys(); },
  };
}

if (typeof module !== "undefined") module.exports = { makePage };

if (typeof document !== "undefined" && typeof window !== "undefined") {
  const page = makePage(document.getElementById("track"));
  window.__racPage = page; // used by page.test.js

  let last = performance.now();
  function tick(now) {
    const dt = Math.max(0, Math.min(0.05, (now - last) / 1000)); // clamp long frames
    last = now;
    page.frame(dt);
    requestAnimationFrame(tick);
  }

  window.addEventListener("keydown", (e) => {
    if (e.key.startsWith("Arrow")) {
      e.preventDefault();
      page.keydown(e.key);
    }
  });
  window.addEventListener("keyup", (e) => {
    if (e.key.startsWith("Arrow")) {
      e.preventDefault();
      page.keyup(e.key);
    }
  });

  requestAnimationFrame(tick);
}
