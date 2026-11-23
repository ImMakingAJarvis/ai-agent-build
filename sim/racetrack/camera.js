// camera.js — the RAC fake camera (docs/sim-racetrack.md, RAC-3).
//
// A small black-and-white pixel grid of the WHOLE circuit seen from directly
// above — a top-down view of everything, not the car's forward view. The
// camera is fixed in the world, and the circuit (the track's geometry)
// determines where it is: its field of view is the track's bounding box
// (track.bounds()). As the car drives, the road stays put and the car — a
// black dot, like the black rectangle on the main canvas — moves across the
// grid, so the logic circuit (RAC-4) sees the track and where the car is in
// it.
//
// Pure function of {car, track}: the page renders it into the "CAM" panel
// (page.js) and the circuit reads it via camera.grid() /
// camera.pixel(row, col). No DOM here — the same code runs in node
// (camera.test.js) and in the browser.
//
// Coordinate convention (fixed — the circuit and the later RAC jobs rely on
// it, so do not change it):
//   - grid[row][col]
//   - row 0 = TOP of the view (world y = minY) ... row (rows-1) = bottom
//   - col 0 = LEFTMOST (world x = minX) ... col (cols-1) = rightmost
//     (the same screen orientation as the main top-down canvas: x grows
//      right, y grows down)
//   - 1 = road/track present at that point, 0 = no road (wall/off-track) —
//     or the car itself (the car is black, so its pixel reads 0)
//
// Each pixel samples one world point in the view:
//
//     x = minX + (col  / (cols - 1)) * (maxX - minX)
//     y = minY + (row  / (rows - 1)) * (maxY - minY)
//
// where {minX, minY, maxX, maxY} is track.bounds() grown by `margin` px, so
// the whole circuit (including its outer road edge) fits in the grid. A
// pixel is road iff track.onRoad(p), the same test the drivable band uses.
// The car is drawn as the single 0 pixel at the sample point nearest its
// center (pass car = null for the road pattern without the car).

"use strict";

const DEFAULTS = {
  cols: 16,        // across the view (col 0 leftmost ... col 15 rightmost)
  rows: 8,         // top to bottom (row 0 top ... row 7 bottom)
  margin: 8,       // px the track's bounding box is grown, per side
};

function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }

// Pure: the rows x cols B/W top-down grid of the whole circuit, with the car
// as a black dot. `car` may be null for the road pattern alone. Returns
// number[][] with grid[row][col] in {0, 1}.
function computeGrid(car, track, opts) {
  const o = Object.assign({}, DEFAULTS, opts || {});
  const { cols, rows, margin } = o;
  const b = track.bounds();
  const minX = b.minX - margin, maxX = b.maxX + margin;
  const minY = b.minY - margin, maxY = b.maxY + margin;
  const sx = (c) => cols > 1 ? minX + (c / (cols - 1)) * (maxX - minX)
                             : (minX + maxX) / 2;
  const sy = (r) => rows > 1 ? minY + (r / (rows - 1)) * (maxY - minY)
                             : (minY + maxY) / 2;
  const grid = new Array(rows);
  for (let r = 0; r < rows; r++) {
    const row = new Array(cols);
    for (let c = 0; c < cols; c++)
      row[c] = track.onRoad(sx(c), sy(r)) ? 1 : 0;
    grid[r] = row;
  }
  // The car: a black dot at the sample point nearest its center.
  if (car) {
    const ci = clamp(Math.round((car.x - minX) / ((maxX - minX) / (cols - 1))),
                     0, cols - 1);
    const ri = clamp(Math.round((car.y - minY) / ((maxY - minY) / (rows - 1))),
                     0, rows - 1);
    grid[ri][ci] = 0;
  }
  return grid;
}

// A live camera bound to a car + track. grid() re-samples on every call, so
// the car's dot tracks the car as it moves (the road stays put); pixel(row,
// col) is the convenience accessor the circuit uses. Both are pure reads of
// the current car state.
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
