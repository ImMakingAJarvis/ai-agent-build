// gates.js — the RAC logic-gate circuit engine (docs/sim-racetrack.md, RAC-4).
//
// A circuit is a small graph of 2-input logic gates (NOT takes 1) whose
// three outputs are the car's controls. Inputs are NAMED sources:
//   - camera pixels  cam.r<row>.c<col>  — one name per pixel of RAC-3's
//     grid (grid[row][col], 1 = road / 0 = wall or car), e.g. cam.r0.c3
//   - constants      const.1, const.0
//   - gates          a gate's id names its output (e.g. "g1")
// Gate ids must not use the cam. / const. namespaces; those names always
// mean pixels and constants.
//
// A circuit object:
//   {
//     gates:   { g1: { type: "AND", in: ["cam.r0.c0", "g2"] },
//                g2: { type: "NOT", in: ["const.0"] } },
//     outputs: { throttle: "g1",      // a gate id or a raw input name
//                steerLeft: "cam.r3.c2",
//                steerRight: "const.1" }
//   }
//
// step(circuit, cameraGrid) -> { throttle, steerLeft, steerRight } is a
// PURE function: topological evaluation of the graph, 0/1 everywhere, no
// DOM, no browser, no mutation of its arguments. The page (RAC-5) calls it
// every frame with camera.grid(); the unit tests (gates.test.js) run the
// same code headless in node. values() is the same evaluation plus every
// gate's live output, for the editor's live-signal view.
//
// Errors (thrown, with a message naming the offending gate/source):
// unknown gate type, wrong input count, unknown input name, a camera
// pixel outside the grid, and cycles.

"use strict";

// The gate set. `inputs` is how many `in` entries the gate needs; `fn`
// returns 0 or 1. Everything is 2-input except NOT (1-input).
const GATES = {
  NOT:  { inputs: 1, fn: (a) => (a ? 0 : 1) },
  AND:  { inputs: 2, fn: (a, b) => (a && b ? 1 : 0) },
  OR:   { inputs: 2, fn: (a, b) => (a || b ? 1 : 0) },
  NAND: { inputs: 2, fn: (a, b) => (a && b ? 0 : 1) },
  NOR:  { inputs: 2, fn: (a, b) => (a || b ? 0 : 1) },
  XOR:  { inputs: 2, fn: (a, b) => ((a ? 1 : 0) === (b ? 1 : 0) ? 0 : 1) },
  XNOR: { inputs: 2, fn: (a, b) => ((a ? 1 : 0) === (b ? 1 : 0) ? 1 : 0) },
};

// The three fixed outputs — the car's controls. Every circuit must drive
// all of them; the car reads all three every frame.
const OUTPUTS = ["throttle", "steerLeft", "steerRight"];

const CAM_RE = /^cam\.r(\d+)\.c(\d+)$/;

// Resolve one named source to 0/1. `memo`/`visiting`/`values` are the
// shared evaluation state (see values()).
function sourceValue(name, circuit, grid, memo, visiting, values) {
  // Pixels and constants first: those namespaces are closed, a name that
  // looks like one always means that (a gate may not be named cam.r0.c0).
  const m = CAM_RE.exec(name);
  if (m) {
    const row = +m[1], col = +m[2];
    if (row >= grid.length || col >= grid[row].length)
      throw new Error("camera pixel out of grid: " + name +
                      " (grid is " + grid.length + "x" + grid[0].length + ")");
    return grid[row][col] ? 1 : 0;
  }
  if (name === "const.1") return 1;
  if (name === "const.0") return 0;
  // Otherwise the source must be one of the circuit's gates.
  const g = circuit.gates[name];
  if (!g) throw new Error("unknown input source: " + name);
  return gateValue(name, circuit, grid, memo, visiting, values);
}

// Evaluate one gate (topologically, with memoization and cycle detection).
function gateValue(id, circuit, grid, memo, visiting, values) {
  if (id in memo) return memo[id];
  if (visiting.has(id)) throw new Error("circuit cycle at gate " + id);
  visiting.add(id);
  const g = circuit.gates[id];
  const def = GATES[g.type];
  if (!def) throw new Error("unknown gate type: " + g.type);
  const ins = g.in;
  if (!Array.isArray(ins) || ins.length !== def.inputs)
    throw new Error("gate " + id + " (" + g.type + ") needs " +
                    def.inputs + " input(s), got " + (Array.isArray(ins) ? ins.length : 0));
  const out = def.fn(...ins.map((s) => sourceValue(s, circuit, grid, memo, visiting, values)));
  visiting.delete(id);
  memo[id] = out;
  values[id] = out;
  return out;
}

// Evaluate the whole circuit: every gate's output plus the three control
// outputs. Pure — reads only, never mutates circuit or grid. Throws on
// any wiring error (see the file header).
function values(circuit, cameraGrid) {
  if (!circuit || typeof circuit !== "object")
    throw new Error("circuit must be an object");
  const gates = circuit.gates || {};
  if (!circuit.outputs) throw new Error("circuit has no outputs");
  for (const k of OUTPUTS)
    if (!(k in circuit.outputs)) throw new Error("circuit is missing output: " + k);
  const grid = cameraGrid;
  if (!Array.isArray(grid) || grid.length === 0 || !Array.isArray(grid[0]))
    throw new Error("cameraGrid must be a non-empty number[][] (grid[row][col])");

  const memo = {};
  const visiting = new Set();
  const out = {};
  for (const id of Object.keys(gates))
    gateValue(id, circuit, grid, memo, visiting, out);
  for (const k of OUTPUTS)
    out[k] = sourceValue(circuit.outputs[k], circuit, grid, memo, visiting, out);
  return out;
}

// One frame of the circuit: the three control outputs, each 0 or 1.
// The pure per-frame entry point the page's auto mode and the tests use.
function step(circuit, cameraGrid) {
  const v = values(circuit, cameraGrid);
  return { throttle: v.throttle, steerLeft: v.steerLeft, steerRight: v.steerRight };
}

if (typeof module !== "undefined")
  module.exports = { GATES, OUTPUTS, values, step };
if (typeof globalThis !== "undefined") {
  globalThis.Racetrack = globalThis.Racetrack || {};
  globalThis.Racetrack.Gates = { GATES, OUTPUTS, values, step };
}
