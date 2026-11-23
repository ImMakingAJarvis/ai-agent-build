// circuit.test.js — headless tests for the RAC-5 circuit model:
// `node --test sim/racetrack/circuit.test.js`. The model is the editor's
// state; these pin its editing operations (add/wire/delete/save/load) and
// prove that a model-built circuit evaluates correctly through the real
// RAC-4 engine (Gates.values / step), so the editor never has to
// reimplement gate logic.

"use strict";

const test = require("node:test");
const assert = require("node:assert");

require("./gates.js"); // sets globalThis.Racetrack.Gates (and module.exports)
const C = require("./circuit.js");
const Gates = require("./gates.js"); // { GATES, OUTPUTS, values, step }

// A real RAC-3 grid: the road ring is 1, the infield is 0 (mirrors
// camera.test.js — here only the shape matters: 8 rows x 16 cols).
const grid = [
  [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0],
  [0, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0],
  [1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1],
  [1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1],
  [1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1],
  [1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1],
  [0, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0],
  [0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0],
];

test("emptyCircuit: no gates, all outputs const.0, evaluates to a parked car", () => {
  const c = C.emptyCircuit();
  assert.deepStrictEqual(Object.keys(c.gates), []);
  assert.deepStrictEqual(
    Gates.step(c, grid),
    { throttle: 0, steerLeft: 0, steerRight: 0 });
});

test("addGate: ids increment, type checked, input count matches the engine", () => {
  const c = C.emptyCircuit();
  assert.strictEqual(C.addGate(c, "AND", 10, 20), "g1");
  assert.strictEqual(C.addGate(c, "NOT", 10, 20), "g2");
  assert.strictEqual(C.addGate(c, "XOR", 10, 20), "g3");
  assert.deepStrictEqual(c.gates.g1.in, ["const.0", "const.0"]);
  assert.deepStrictEqual(c.gates.g2.in, ["const.0"]);
  assert.throws(() => C.addGate(c, "XOR2", 0, 0), /unknown gate type: XOR2/);
  assert.throws(() => C.addGate(c, "AND", 0, 0, "g1"), /already used/);
});

test("setGateInput: validates slot, source and self-reference", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "AND"); // g1
  C.addGate(c, "OR");  // g2
  C.setGateInput(c, "g1", 0, "cam.r0.c2");
  C.setGateInput(c, "g1", 1, "g2");
  assert.deepStrictEqual(c.gates.g1.in, ["cam.r0.c2", "g2"]);
  assert.throws(() => C.setGateInput(c, "g1", 2, "const.1"), /no input slot 2/);
  assert.throws(() => C.setGateInput(c, "g1", 0, "nope"), /unknown input source/);
  assert.throws(() => C.setGateInput(c, "g2", 0, "g2"), /cannot feed itself/);
  assert.throws(() => C.setGateInput(c, "g9", 0, "const.1"), /unknown gate: g9/);
});

test("setOutput: validates the three names and the source", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "NOT");
  C.setOutput(c, "throttle", "g1");
  assert.throws(() => C.setOutput(c, "brake", "g1"), /unknown output: brake/);
  assert.throws(() => C.setOutput(c, "throttle", "missing"), /unknown output source/);
  assert.strictEqual(C.setOutput(c, "steerLeft", "cam.r7.c15"), undefined);
});

test("cycles are refused at wire time", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "AND"); // g1
  C.addGate(c, "AND"); // g2
  C.setGateInput(c, "g1", 0, "const.1");
  C.setGateInput(c, "g1", 1, "const.1");
  // g2 may consume g1; wiring g1 back to g2 would close the loop.
  C.setGateInput(c, "g2", 0, "g1");
  C.setGateInput(c, "g2", 1, "const.1");
  assert.throws(() => C.setGateInput(c, "g1", 0, "g2"), /cycle/);
  C.setOutput(c, "throttle", "g1");
  // Still refused with the output wired in (the check is on the gate
  // fan-in; outputs are sinks and never sources).
  assert.throws(() => C.setGateInput(c, "g1", 1, "g2"), /cycle/);
  // Self-loops through a single gate.
  const c2 = C.emptyCircuit();
  C.addGate(c2, "NOT");
  assert.throws(() => C.setGateInput(c2, "g1", 0, "g1"), /cannot feed itself/);
});

test("transitive cycle through three gates is refused", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "OR"); // g1
  C.addGate(c, "OR"); // g2
  C.addGate(c, "OR"); // g3
  C.setGateInput(c, "g1", 0, "g2"); C.setGateInput(c, "g1", 1, "const.0");
  C.setGateInput(c, "g2", 0, "g3"); C.setGateInput(c, "g2", 1, "const.0");
  assert.throws(() => C.setGateInput(c, "g3", 0, "g1"), /cycle/);
});

test("deleteGate: removes the gate and rewires dependents to const.0", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "AND"); // g1
  C.addGate(c, "OR");  // g2
  C.setGateInput(c, "g2", 0, "g1");
  C.setOutput(c, "throttle", "g1");
  C.deleteGate(c, "g1");
  assert.deepStrictEqual(Object.keys(c.gates), ["g2"]);
  assert.strictEqual(c.gates.g2.in[0], "const.0");
  assert.strictEqual(c.outputs.throttle, "const.0");
  // Still evaluatable after the deletion.
  assert.deepStrictEqual(Gates.step(c, grid),
    { throttle: 0, steerLeft: 0, steerRight: 0 });
  assert.throws(() => C.deleteGate(c, "g9"), /unknown gate: g9/);
});

test("toJSON / fromJSON: round-trip keeps a working circuit identical", () => {
  const c = C.emptyCircuit();
  C.addGate(c, "OR", 12, 34);
  C.setGateInput(c, "g1", 0, "cam.r0.c2"); // road (1) in the test grid
  C.setGateInput(c, "g1", 1, "const.0");
  C.setOutput(c, "throttle", "g1");
  const text = C.toJSON(c);
  const c2 = C.fromJSON(text);
  assert.deepStrictEqual(c2, c);
  // And the re-loaded copy evaluates through the real engine.
  assert.strictEqual(Gates.step(c2, grid).throttle, 1, "OR(road, 0) = 1");
});

test("save/load: a model-built circuit drives the engine exactly", () => {
  // throttle = cam.r0.c2 AND const.1 (road pixel), the rest const.0.
  const c = C.emptyCircuit();
  C.addGate(c, "AND");
  C.setGateInput(c, "g1", 0, "cam.r0.c2");
  C.setGateInput(c, "g1", 1, "const.1");
  C.setOutput(c, "throttle", "g1");
  const v = Gates.values(c, grid);
  assert.strictEqual(v.throttle, 1, "road pixel drives throttle");
  const off = grid.map((row) => row.slice());
  off[0][2] = 0; // that pixel is wall now
  assert.strictEqual(Gates.step(c, off).throttle, 0,
    "the same circuit stops driving when the pixel sees wall");
});

test("fromJSON: rejects bad JSON, wrong shapes and unresolvable sources", () => {
  assert.throws(() => C.fromJSON("{nope"), /not valid JSON/);
  assert.throws(() => C.fromJSON(JSON.stringify({ gates: {} })),
    /circuit has no outputs/);
  assert.throws(() => C.fromJSON(JSON.stringify({
    gates: {},
    outputs: { steerLeft: "const.0", steerRight: "const.0" },
  })), /missing output: throttle/);
  assert.throws(() => C.fromJSON(JSON.stringify({
    gates: { g1: { type: "AND", in: ["cam.r0.c0"], x: 0, y: 0 } },
    outputs: { throttle: "g1", steerLeft: "const.0", steerRight: "const.0" },
  })), /needs 2 input\(s\), got 1/);
  assert.throws(() => C.fromJSON(JSON.stringify({
    gates: { g1: { type: "AND", in: ["ghost", "const.0"] } },
    outputs: { throttle: "g1", steerLeft: "const.0", steerRight: "const.0" },
  })), /unknown input source: ghost/);
  // A cycle hidden in JSON is caught by validate.
  assert.throws(() => C.fromJSON(JSON.stringify({
    gates: { g1: { type: "OR", in: ["g2", "const.0"] },
             g2: { type: "OR", in: ["g1", "const.0"] } },
    outputs: { throttle: "g1", steerLeft: "const.0", steerRight: "const.0" },
  })), /cycle/);
});
