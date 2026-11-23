// camera.js — the RAC fake camera (docs/sim-racetrack.md, RAC-3).
//
// A small black-and-white pixel grid of the scene ahead of the car, as seen
// from the car. It is a pure function of {car, track}: the page renders it
// into the "CAM" panel (page.js) and the logic circuit (RAC-4) reads it via
// camera.grid() / camera.pixel(row, col). No DOM here — the same code runs in
// node (camera.test.js) and in the browser.
//
// Coordinate convention (fixed — the circuit and the later RAC jobs rely on
// it, so do not change it):
//   - grid[row][col]
//   - row 0 = NEAREST to the car ... row (rows-1) = farthest (depth)
//   - col 0 = LEFTMOST in the view ... col (cols-1) = rightmost
//   - 1 = road/track present at that point, 0 = no road (wall / off-track)
//
// Each pixel samples one world point in the car's forward view:
//
//     p = car.pos + forward * depth(row) + right * lateral(col)
//
// where forward = (cos h, sin h) is the car's heading and right is the
// car's right side, 90° clockwise from forward (canvas y grows down, so
// right = (-sin h, cos h) — checked against all four cardinal headings).
// A pixel is road iff track.onRoad(p), the same test the drivable band uses,
// so the camera sees exactly the road the car can drive on.

"use strict";

const DEFAULTS = {
  cols: 16,        // across the field of view (col 0 leftmost ... col 15 rightmost)
  rows: 8,         // depth (row 0 nearest ... row 7 farthest)
  nearDist: 12,    // px ahead of the car sampled by row 0
  farDist: 220,    // px ahead of the car sampled by the last row
  lateralHalf: 80, // px to the left/right edge sampled by col 0 / the last col
};

// Pure: the rows x cols B/W grid for a car at (car.x, car.y) with
// car.heading, given a track (it only needs track.onRoad(x, y)). Returns
// number[][] with grid[row][col] in {0, 1}.
function computeGrid(car, track, opts) {
  const o = Object.assign({}, DEFAULTS, opts || {});
  const { cols, rows, nearDist, farDist, lateralHalf } = o;
  const fx = Math.cos(car.heading);
  const fy = Math.sin(car.heading);
  const rx = -Math.sin(car.heading); // car's right side (canvas y grows down)
  const ry = Math.cos(car.heading);
  const grid = new Array(rows);
  for (let r = 0; r < rows; r++) {
    const depth = nearDist + (r / (rows - 1)) * (farDist - nearDist);
    const row = new Array(cols);
    for (let c = 0; c < cols; c++) {
      const lateral = 2 * lateralHalf * (c / (cols - 1) - 0.5);
      const px = car.x + fx * depth + rx * lateral;
      const py = car.y + fy * depth + ry * lateral;
      row[c] = track.onRoad(px, py) ? 1 : 0;
    }
    grid[r] = row;
  }
  return grid;
}

// A live camera bound to a car + track. grid() re-samples on every call, so
// it tracks the car as it moves and turns; pixel(row, col) is the
// convenience accessor the circuit uses. Both are pure reads of the current
// car state.
function makeCamera(car, track, opts) {
  const o = Object.assign({}, DEFAULTS, opts || {});
  function grid() { return computeGrid(car, track, o); }
  function pixel(row, col) { return grid()[row][col]; }
  return { grid, pixel, cols: o.cols, rows: o.rows };
}

if (typeof module !== "undefined")
  module.exports = { computeGrid, makeCamera, DEFAULTS };
if (typeof globalThis !== "undefined") {
  globalThis.Racetrack = globalThis.Racetrack || {};
  globalThis.Racetrack.Camera = { computeGrid, makeCamera, DEFAULTS };
}
