// camera.test.js — headless unit tests for the RAC fake camera (RAC-3).
//
// `node --test sim/racetrack/camera.test.js` (node >= 18; node v24 on the
// box, no npm deps). Runs the real camera.js + sim.js in node (no DOM).
// Pins the coordinate convention the circuit (RAC-4) and later RAC jobs
// rely on: a TOP-DOWN view of the whole circuit, framed on the track's
// bounding box; grid[row][col], row 0 = top, col 0 = leftmost; 1 = road,
// 0 = wall/off-track or the car (the car is a black dot at the sample
// point nearest its center).

"use strict";

const { test } = require("node:test");
const assert = require("node:assert");
const { Track, Car } = require("./sim.js");
const { computeGrid, makeCamera, DEFAULTS } = require("./camera.js");

// The car's start (page.js): on the centerline of the bottom-right corner,
// facing along the bottom straight (-x).
function startCar() { return Car.create(Track.a, Track.r, Math.PI); }

// How many pixels differ between two grids of the same shape.
function diffCount(a, b) {
  let n = 0;
  for (let r = 0; r < a.length; r++)
    for (let c = 0; c < a[r].length; c++)
      if (a[r][c] !== b[r][c]) n++;
  return n;
}

test("default grid is 8 rows x 16 cols, values only 0 or 1", () => {
  const g = computeGrid(startCar(), Track);
  assert.strictEqual(g.length, 8, "8 rows");
  for (const row of g) {
    assert.strictEqual(row.length, 16, "16 cols");
    for (const v of row) assert.ok(v === 0 || v === 1, "value is 0 or 1");
  }
});

test("track bounds are the circuit's box (the camera frames on it)", () => {
  assert.deepStrictEqual(Track.bounds(), {
    minX: -392, maxX: 392, minY: -172, maxY: 172,
  });
});

test("the grid is a top-down map of the whole circuit (road ring, empty infield)", () => {
  const g = computeGrid(null, Track); // car = null: the road pattern alone
  // Outermost rows sit past the outer road edge (the margin grows the box):
  for (let c = 0; c < 16; c++)
    assert.strictEqual(g[0][c], 0, `row 0 col ${c} is off-track`);
  assert.deepStrictEqual(g[7], g[0], "row 7 mirrors row 0");
  // The top straight (row 1): road across the middle, wall at the ends.
  for (const c of [0, 1, 14, 15])
    assert.strictEqual(g[1][c], 0, `row 1 col ${c} is wall`);
  for (let c = 2; c <= 13; c++)
    assert.strictEqual(g[1][c], 1, `row 1 col ${c} is the top straight`);
  // The middle rows (3/4): only the left/right arcs are road; the infield
  // and the far ends are wall.
  for (const r of [3, 4]) {
    for (const c of [0, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 15])
      assert.strictEqual(g[r][c], 0, `row ${r} col ${c} is infield/wall`);
    for (const c of [1, 2, 13, 14])
      assert.strictEqual(g[r][c], 1, `row ${r} col ${c} is an arc`);
  }
  // The stadium is symmetric: left-right and top-bottom.
  for (let r = 0; r < 8; r++)
    for (let c = 0; c < 16; c++) {
      assert.strictEqual(g[r][c], g[r][15 - c], "left-right symmetry");
      assert.strictEqual(g[r][c], g[7 - r][c], "top-bottom symmetry");
    }
});

test("the car is a black dot at the sample point nearest its center", () => {
  const road = computeGrid(null, Track);
  const g = computeGrid(startCar(), Track);
  // (220, 120): col = round((220+400)/53.33) = 12, row = round((120+180)/51.43)
  // = 6 — exactly one pixel flips from road (1) to the car (0).
  assert.strictEqual(diffCount(road, g), 1, "the car flips exactly one pixel");
  assert.strictEqual(g[6][12], 0, "the dot pixel reads 0");
  assert.strictEqual(road[6][12], 1, "the dot sits on the road");
  assert.strictEqual(g[5][12], 1, "road next to the dot still reads 1");
  assert.strictEqual(g[6][11], 1, "road next to the dot still reads 1");
});

test("moving the car moves the dot; the road pattern stays put", () => {
  const atStart = computeGrid(startCar(), Track);
  // Same car, mirrored to the opposite corner of the circuit: (-220, -120)
  // quantizes to (row 1, col 3) — on the top straight.
  const far = computeGrid({ x: -Track.a, y: -Track.r, heading: 0, speed: 0 }, Track);
  assert.strictEqual(diffCount(atStart, far), 2,
    "only the two dot pixels differ (old dot back to 1, new dot to 0)");
  assert.strictEqual(far[1][3], 0, "the dot moved to the new corner");
  // A top-down view does not rotate with the car: heading changes nothing.
  const h0 = computeGrid({ x: 220, y: 120, heading: 0, speed: 0 }, Track);
  const hPi = computeGrid({ x: 220, y: 120, heading: Math.PI, speed: 0 }, Track);
  assert.deepStrictEqual(h0, hPi, "the grid is independent of the car's heading");
});

test("the whole circuit is inside the view (a corner car still has a dot)", () => {
  // Far-left arc, on the centerline: (-340, 0) quantizes to (row 4, col 1)
  // (it sits exactly halfway between rows 3 and 4; Math.round halves up).
  const car = Car.create(-Track.a - Track.r, 0, 0);
  const road = computeGrid(null, Track);
  const g = computeGrid(car, Track);
  assert.strictEqual(diffCount(road, g), 1, "the corner car flips one pixel");
  assert.strictEqual(g[4][1], 0, "the dot is inside the grid, on the left arc");
});

test("makeCamera: grid() and pixel(row,col) agree and follow the car", () => {
  const car = startCar();
  const cam = makeCamera(car, Track);
  assert.strictEqual(cam.cols, 16);
  assert.strictEqual(cam.rows, 8);
  const g = cam.grid();
  for (let r = 0; r < 8; r++)
    for (let c = 0; c < 16; c++)
      assert.strictEqual(cam.pixel(r, c), g[r][c], `pixel(${r},${c}) matches grid()`);
  // Move the car to the opposite corner: grid() must re-sample and the old
  // dot pixel must flip back to road.
  car.x = -Track.a;
  car.y = -Track.r;
  assert.strictEqual(cam.pixel(6, 12), 1, "the old dot pixel is road again");
  assert.strictEqual(cam.pixel(1, 3), 0, "the dot followed the car");
});

test("custom sizes are honored", () => {
  const g = computeGrid(startCar(), Track, { cols: 4, rows: 2 });
  assert.strictEqual(g.length, 2, "2 rows");
  for (const row of g) assert.strictEqual(row.length, 4, "4 cols");
  assert.deepStrictEqual(DEFAULTS, { cols: 16, rows: 8, margin: 8 });
});
