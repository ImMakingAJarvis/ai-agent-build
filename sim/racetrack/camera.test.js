// camera.test.js — headless unit tests for the RAC fake camera (RAC-3).
//
// `node --test sim/racetrack/camera.test.js` (node >= 18; node v24 on the
// box, no npm deps). Runs the real camera.js + sim.js in node (no DOM).
// Pins the coordinate convention the circuit (RAC-4) and later RAC jobs rely
// on: grid[row][col], row 0 = nearest, col 0 = leftmost, 1 = road, 0 = wall.

"use strict";

const { test } = require("node:test");
const assert = require("node:assert");
const { Track, Car } = require("./sim.js");
const { computeGrid, makeCamera, DEFAULTS } = require("./camera.js");

// A car on the centerline of the bottom straight, facing along it (-x).
function straightCar() { return Car.create(0, Track.r, Math.PI); }

test("default grid is 8 rows x 16 cols, values only 0 or 1", () => {
  const g = computeGrid(straightCar(), Track);
  assert.strictEqual(g.length, 8, "8 rows");
  for (const row of g) {
    assert.strictEqual(row.length, 16, "16 cols");
    for (const v of row) assert.ok(v === 0 || v === 1, "value is 0 or 1");
  }
});

test("on a straight: center columns are road, outer columns are wall", () => {
  const g = computeGrid(straightCar(), Track);
  for (let r = 0; r < 8; r++) {
    assert.strictEqual(g[r][7], 1, `row ${r} col 7 (center) is road`);
    assert.strictEqual(g[r][8], 1, `row ${r} col 8 (center) is road`);
    assert.strictEqual(g[r][0], 0, `row ${r} col 0 (leftmost) is wall`);
    assert.strictEqual(g[r][15], 0, `row ${r} col 15 (rightmost) is wall`);
  }
  // The drivable band is exactly the middle ten columns here (lateral +-52
  // within the +-80 field of view).
  for (let r = 0; r < 8; r++) {
    for (let c = 3; c <= 12; c++)
      assert.strictEqual(g[r][c], 1, `row ${r} col ${c} is road`);
    for (const c of [0, 1, 2, 13, 14, 15])
      assert.strictEqual(g[r][c], 0, `row ${r} col ${c} is wall`);
  }
});

test("after a 90-degree turn the pattern shifts to match", () => {
  const pos = { x: 0, y: Track.r }; // mid-bottom-straight, on the centerline
  const along = computeGrid({ ...pos, heading: Math.PI }, Track);
  const across = computeGrid({ ...pos, heading: Math.PI + Math.PI / 2 }, Track);
  // Same car spot, nose turned 90 degrees into the infield: a mid-depth
  // center pixel that was road (straight ahead) is now wall (the infield),
  // so the pattern shifted with the heading.
  assert.strictEqual(along[4][7], 1, "facing along the straight: mid-depth center is road");
  assert.strictEqual(across[4][7], 0, "after the 90-degree turn: the same pixel is the infield (wall)");
  assert.notDeepStrictEqual(along, across, "a 90-degree turn changes the grid");
});

test("facing a wall: the grid is (mostly) all 0", () => {
  // On the outer edge, nose pointed straight out at the wall: no road ahead.
  const car = Car.create(0, Track.r + Track.halfW - 2, Math.PI / 2);
  const g = computeGrid(car, Track);
  let ones = 0;
  for (const row of g) for (const v of row) if (v === 1) ones++;
  assert.strictEqual(ones, 0, `facing the outer wall, no road ahead (found ${ones} road pixels)`);
});

test("makeCamera: grid() and pixel(row,col) agree and follow the car", () => {
  const car = straightCar();
  const cam = makeCamera(car, Track);
  assert.strictEqual(cam.cols, 16);
  assert.strictEqual(cam.rows, 8);
  const g = cam.grid();
  for (let r = 0; r < 8; r++)
    for (let c = 0; c < 16; c++)
      assert.strictEqual(cam.pixel(r, c), g[r][c], `pixel(${r},${c}) matches grid()`);
  // Turn the car 90 degrees: the grid must follow.
  const before = cam.grid()[4][7];
  car.heading = Math.PI + Math.PI / 2;
  const after = cam.grid()[4][7];
  assert.notStrictEqual(before, after, "grid() reflects the car's new heading");
});

test("custom sizes are honored", () => {
  const g = computeGrid(straightCar(), Track, { cols: 4, rows: 2 });
  assert.strictEqual(g.length, 2, "2 rows");
  for (const row of g) assert.strictEqual(row.length, 4, "4 cols");
  assert.deepStrictEqual(DEFAULTS, { cols: 16, rows: 8, nearDist: 12, farDist: 220, lateralHalf: 80 });
});
