// page.js — the browser layer of the RAC page: canvas, arrow keys, the
// fake-camera "CAM" panel and the requestAnimationFrame loop. All physics
// and the camera math live in sim.js / camera.js (pure); this file is the
// thin DOM wiring on top of them.
//
// Input: ArrowUp = throttle, ArrowDown = brake/reverse,
//        ArrowLeft / ArrowRight = steer.

"use strict";

function makePage(canvas, camCanvas) {
  const { Track, Car, Camera } = globalThis.Racetrack;
  const ctx = canvas.getContext("2d");
  const W = Track.canvasW;
  const H = Track.canvasH;
  canvas.width = W;
  canvas.height = H;

  // The car starts on the bottom straight, on the centerline, facing -x
  // (canvas y grows down, so the bottom of the track is +y).
  const car = Car.create(Track.a, Track.r, Math.PI);

  // The fake camera: a pure read of the car + track (camera.js) — a
  // top-down view of the whole circuit, framed on the track's bounding box.
  // grid() re-samples every call, so the car's dot follows the car.
  const camera = Camera.makeCamera(car, Track);

  // The CAM panel: render the B/W grid to its own small canvas.
  // Top-down orientation, like the main canvas: row 0 (world top) at the
  // top, col 0 (leftmost) on the left. 1 = road -> white, 0 = wall ->
  // black; the car's black dot is part of the grid, so the panel updates
  // as the car drives.
  const CELL = 16;
  let camCtx = null;
  if (camCanvas) {
    camCtx = camCanvas.getContext("2d");
    camCanvas.width = camera.cols * CELL;
    camCanvas.height = camera.rows * CELL;
  }

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

    drawCam();
  }

  // Paint the fake camera's B/W grid into the CAM panel (one block per
  // pixel; only the two colors #000 / #fff), top-down orientation.
  function drawCam() {
    if (!camCtx) return;
    const g = camera.grid();
    for (let r = 0; r < camera.rows; r++) {
      for (let c = 0; c < camera.cols; c++) {
        camCtx.fillStyle = g[r][c] ? "#fff" : "#000";
        camCtx.fillRect(c * CELL, r * CELL, CELL, CELL);
      }
    }
  }

  function frame(dt) {
    Car.step(car, input, dt);
    draw();
  }

  draw(); // paint the first frame immediately

  return {
    car,
    input,
    camera,
    frame,
    draw,
    keydown(code) { keys.add(code); readKeys(); },
    keyup(code) { keys.delete(code); readKeys(); },
  };
}

if (typeof module !== "undefined") module.exports = { makePage };

if (typeof document !== "undefined" && typeof window !== "undefined") {
  const page = makePage(document.getElementById("track"),
                        document.getElementById("cam"));
  window.__racPage = page; // used by page.test.js
  window.__racCamera = page.camera; // the circuit (RAC-4) reads grid()/pixel()

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
