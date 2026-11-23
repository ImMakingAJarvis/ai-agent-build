// gates.test.js — headless unit tests for the RAC logic-gate engine (RAC-4).
//
// `node --test sim/racetrack/gates.test.js` (node >= 18; node v24 on the
// box, no npm deps). Runs the real gates.js in node (no DOM) and pins:
//   - the full truth table of every gate in the set,
//   - step(circuit, grid) -> { throttle, steerLeft, steerRight } on a
//     hand-built circuit with known outputs,
//   - outputs wired straight from inputs (a raw camera pixel, a constant),
//   - that step/values are pure (they do not mutate their arguments),
//   - that cam.r<row>.c<col> names address RAC-3's real camera grid,
//   - the wiring errors (cycle, unknown gate type, wrong input count,
//     unknown source, pixel out of grid, missing output).

"use strict";

const { test } = require("node:test");
const assert = require("node:assert");
const { GATES, OUTPUTS, values, step } = require("./gates.js");
const { Track } = require("./sim.js");
const { computeGrid } = require("./camera.js");

// ---- the gate set itself -------------------------------------------------

test("the gate set is exactly NOT/AND/OR/NAND/NOR/XOR/XNOR with the right arities", () => {
  assert.deepStrictEqual(Object.keys(GATES).sort(),
                         ["AND", "NAND", "NOR", "NOT", "OR", "XNOR", "XOR"]);
  assert.strictEqual(GATES.NOT.inputs, 1);
  for (const t of ["AND", "OR", "NAND", "NOR", "XOR", "XNOR"])
    assert.strictEqual(GATES[t].inputs, 2, t + " is 2-input");
});

test("the fixed outputs are the three car controls", () => {
  assert.deepStrictEqual(OUTPUTS, ["throttle", "steerLeft", "steerRight"]);
});

// Full truth tables: [inputs..., expected].
const TABLES = {
  NOT:  [[0, 1], [1, 0]],
  AND:  [[0, 0, 0], [0, 1, 0], [1, 0, 0], [1, 1, 1]],
  OR:   [[0, 0, 0], [0, 1, 1], [1, 0, 1], [1, 1, 1]],
  NAND: [[0, 0, 1], [0, 1, 1], [1, 0, 1], [1, 1, 0]],
  NOR:  [[0, 0, 1], [0, 1, 0], [1, 0, 0], [1, 1, 0]],
  XOR:  [[0, 0, 0], [0, 1, 1], [1, 0, 1], [1, 1, 0]],
  XNOR: [[0, 0, 1], [0, 1, 0], [1, 0, 0], [1, 1, 1]],
};
for (const [type, rows] of Object.entries(TABLES)) {
  test("truth table: " + type, () => {
    const fn = GATES[type].fn;
    for (const row of rows) {
      const want = row[row.length - 1];
      const got = fn(...row.slice(0, -1));
      assert.strictEqual(got, want, type + "(" + row.slice(0, -1).join(", ") + ")");
    }
  });
}

// ---- a small hand-built circuit with known outputs -----------------------
//
// Grid (3 rows x 4 cols):
//   r0: 1 1 0 0
//   r1: 0 1 1 0
//   r2: 1 0 0 1
//
//   gL    = OR(cam.r0.c0,  cam.r0.c1)  -> 1  (road on the left half)
//   gR    = AND(cam.r0.c2, cam.r0.c3)  -> 0  (no road on the right half)
//   gBoth = NAND(gL, gR)               -> 1
//   gDiff = XOR(gL, gR)                -> 1
//
//   throttle   = gBoth        -> 1
//   steerLeft  = gDiff        -> 1
//   steerRight = cam.r1.c3    -> 0   (wired straight from a pixel)

const HAND_GRID = [
  [1, 1, 0, 0],
  [0, 1, 1, 0],
  [1, 0, 0, 1],
];
const HAND_CIRCUIT = {
  gates: {
    gL:    { type: "OR",   in: ["cam.r0.c0", "cam.r0.c1"] },
    gR:    { type: "AND",  in: ["cam.r0.c2", "cam.r0.c3"] },
    gBoth: { type: "NAND", in: ["gL", "gR"] },
    gDiff: { type: "XOR",  in: ["gL", "gR"] },
  },
  outputs: { throttle: "gBoth", steerLeft: "gDiff", steerRight: "cam.r1.c3" },
};

test("hand-built circuit: step gives the known control outputs", () => {
  assert.deepStrictEqual(step(HAND_CIRCUIT, HAND_GRID),
                         { throttle: 1, steerLeft: 1, steerRight: 0 });
});

test("hand-built circuit: values carries every gate's live output too", () => {
  assert.deepStrictEqual(values(HAND_CIRCUIT, HAND_GRID), {
    gL: 1, gR: 0, gBoth: 1, gDiff: 1,
    throttle: 1, steerLeft: 1, steerRight: 0,
  });
});

test("step is pure: it does not mutate the circuit or the grid", () => {
  const grid = HAND_GRID.map((r) => r.slice());
  const circuit = JSON.parse(JSON.stringify(HAND_CIRCUIT));
  const gridBefore = JSON.stringify(grid);
  const circuitBefore = JSON.stringify(circuit);
  step(circuit, grid);
  values(circuit, grid);
  assert.strictEqual(JSON.stringify(grid), gridBefore, "grid untouched");
  assert.strictEqual(JSON.stringify(circuit), circuitBefore, "circuit untouched");
});

// ---- outputs wired straight from inputs ----------------------------------

test("an output can be a raw camera pixel or a constant (no gates at all)", () => {
  const c = {
    gates: {},
    outputs: { throttle: "cam.r0.c0", steerLeft: "const.1", steerRight: "const.0" },
  };
  assert.deepStrictEqual(step(c, [[1, 0], [0, 1]]),
                         { throttle: 1, steerLeft: 1, steerRight: 0 });
  assert.deepStrictEqual(step(c, [[0, 0], [0, 1]]),
                         { throttle: 0, steerLeft: 1, steerRight: 0 });
});

// ---- cam names address RAC-3's real grid ---------------------------------

test("cam.r<row>.c<col> names address the real camera grid (grid[row][col])", () => {
  const grid = computeGrid(null, Track); // the road pattern, no car
  // The pinned camera (camera.test.js) is a road ring with an empty
  // infield, so the grid has both 1 and 0 pixels — find one of each.
  let one = null, zero = null;
  for (let r = 0; r < grid.length && (!one || !zero); r++)
    for (let c = 0; c < grid[r].length && (!one || !zero); c++) {
      if (grid[r][c] === 1 && !one) one = { r, c };
      if (grid[r][c] === 0 && !zero) zero = { r, c };
    }
  assert.ok(one && zero, "the grid has road and non-road pixels");
  const c = {
    gates: {},
    outputs: {
      throttle:   "cam.r" + one.r + ".c" + one.c,
      steerLeft:  "cam.r" + zero.r + ".c" + zero.c,
      steerRight: "const.1",
    },
  };
  assert.deepStrictEqual(step(c, grid),
                         { throttle: 1, steerLeft: 0, steerRight: 1 });
});

// ---- wiring errors ---------------------------------------------------------

const GRID = [[1, 0]];

test("a cycle in the gates throws", () => {
  const c = {
    gates: { a: { type: "NOT", in: ["b"] }, b: { type: "NOT", in: ["a"] } },
    outputs: { throttle: "a", steerLeft: "a", steerRight: "a" },
  };
  assert.throws(() => step(c, GRID), /cycle/);
});

test("an unknown gate type throws", () => {
  const c = {
    gates: { g1: { type: "IMP", in: ["const.1", "const.0"] } },
    outputs: { throttle: "g1", steerLeft: "g1", steerRight: "g1" },
  };
  assert.throws(() => step(c, GRID), /unknown gate type/);
});

test("a wrong input count throws", () => {
  const c = {
    gates: { g1: { type: "NOT", in: ["const.1", "const.0"] } },
    outputs: { throttle: "g1", steerLeft: "g1", steerRight: "g1" },
  };
  assert.throws(() => step(c, GRID), /needs 1 input/);
});

test("an unknown input source throws", () => {
  const c = {
    gates: {},
    outputs: { throttle: "nope", steerLeft: "const.1", steerRight: "const.0" },
  };
  assert.throws(() => step(c, GRID), /unknown input source/);
});

test("a camera pixel outside the grid throws", () => {
  const c = {
    gates: {},
    outputs: { throttle: "cam.r9.c0", steerLeft: "const.1", steerRight: "const.0" },
  };
  assert.throws(() => step(c, GRID), /out of grid/);
});

test("a missing control output throws", () => {
  const c = { gates: {}, outputs: { throttle: "const.1", steerLeft: "const.0" } };
  assert.throws(() => step(c, GRID), /missing output/);
});
