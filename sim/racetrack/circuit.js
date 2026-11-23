// circuit.js — the RAC circuit model (docs/sim-racetrack.md, RAC-5).
//
// The gate engine (gates.js) EVALUATES a circuit; this file owns the
// EDITING of one: create it, place gates, wire inputs and outputs, delete
// gates, and save/load it as JSON. The circuit object is exactly the plain
// data shape gates.js consumes —
//   { gates:   { id: { type, in: [source, ...] } },
//     outputs: { throttle: source, steerLeft: source, steerRight: source } }
// — plus an editor-only { x, y } on each gate (its position in the editor
// canvas; the engine ignores it). A source is a gate id, a camera pixel
// name cam.r<row>.c<col>, or const.1 / const.0.
//
// Pure: no DOM, no browser, no mutation of anything but the circuit object
// passed in. The editor UI (editor.js) never reimplements gate logic — it
// mutates the model through these operations and asks Gates.values() for
// the live signals. Every operation that would leave the circuit
// un-evaluatable throws with a message naming the offender, so the UI can
// show the error instead of a silently broken car.

"use strict";

const CAM_RE = /^cam\.r(\d+)\.c(\d+)$/;

// A source name the editor may accept anywhere a wire can go: constants,
// camera pixels (any row/col — range is checked against the live grid by
// the engine at eval time), or an existing gate id.
function isSourceName(name, gates) {
  if (name === "const.0" || name === "const.1") return true;
  if (CAM_RE.test(name)) return true;
  return typeof name === "string" && !!gates[name];
}

// True when `target` appears in the transitive fan-in of `node` (node's
// inputs, then their inputs, ...). Wiring source -> gateId creates a cycle
// exactly when gateId is already in source's fan-in.
function inFanIn(node, target, gates, seen) {
  if (seen.has(node)) return false;
  seen.add(node);
  const g = gates[node];
  if (!g) return false;
  for (const s of g.in) {
    if (s === target) return true;
    if (gates[s] && inFanIn(s, target, gates, seen)) return true;
  }
  return false;
}

// The starting circuit: no gates, all three outputs hard-wired to const.0
// so AUTO mode is a parked car until someone builds something.
function emptyCircuit() {
  return {
    gates: {},
    outputs: { throttle: "const.0", steerLeft: "const.0", steerRight: "const.0" },
  };
}

// Next free gate id: g1, g2, ... (never reuses deleted ids while they are
// still referenced elsewhere; simple "max + 1" is good enough here).
function nextId(gates) {
  let n = 0;
  for (const id of Object.keys(gates)) {
    const m = /^g(\d+)$/.exec(id);
    if (m && +m[1] > n) n = +m[1];
  }
  return "g" + (n + 1);
}

// Place a gate. type must be a real gate type (checked against the engine's
// GATES table, not re-listed here). x/y are editor-canvas coordinates.
function addGate(circuit, type, x, y, id) {
  const { GATES } = requireGates();
  if (!GATES[type]) throw new Error("unknown gate type: " + type);
  id = id || nextId(circuit.gates);
  if (circuit.gates[id]) throw new Error("gate id already used: " + id);
  circuit.gates[id] = {
    type,
    in: new Array(GATES[type].inputs).fill("const.0"),
    x: Math.round(x || 0),
    y: Math.round(y || 0),
  };
  return id;
}

// Remove a gate. Everything that referenced it (other gates' inputs, the
// outputs) is rewired to const.0, so the circuit stays evaluatable.
function deleteGate(circuit, id) {
  if (!circuit.gates[id]) throw new Error("unknown gate: " + id);
  for (const g of Object.values(circuit.gates))
    for (let i = 0; i < g.in.length; i++)
      if (g.in[i] === id) g.in[i] = "const.0";
  for (const k of Object.keys(circuit.outputs))
    if (circuit.outputs[k] === id) circuit.outputs[k] = "const.0";
  delete circuit.gates[id];
}

function requireGates() {
  const g = globalThis.Racetrack && globalThis.Racetrack.Gates;
  if (!g) throw new Error("gates.js must load before circuit.js");
  return g;
}

// True when `source` names one of this circuit's gates.
function isGateId(circuit, source) { return !!circuit.gates[source]; }

// Wire one input slot of a gate. slot 0..(arity-1); source validated and
// cycle-checked. Throws on unknown gate/slot/source, self-reference, or a
// cycle the new wire would create.
function setGateInput(circuit, gateId, slot, source) {
  const { GATES } = requireGates();
  const g = circuit.gates[gateId];
  if (!g) throw new Error("unknown gate: " + gateId);
  const def = GATES[g.type];
  if (!Number.isInteger(slot) || slot < 0 || slot >= def.inputs)
    throw new Error("gate " + gateId + " (" + g.type + ") has no input slot " + slot);
  if (!isSourceName(source, circuit.gates))
    throw new Error("unknown input source: " + source);
  if (source === gateId) throw new Error("gate cannot feed itself: " + gateId);
  if (isGateId(circuit, source) && inFanIn(source, gateId, circuit.gates, new Set()))
    throw new Error("wiring " + source + " -> " + gateId + " would create a cycle");
  g.in[slot] = source;
}

// Wire one of the three car-control outputs to a source. Same validation
// as setGateInput.
function setOutput(circuit, name, source) {
  const { OUTPUTS } = requireGates();
  if (!OUTPUTS.includes(name)) throw new Error("unknown output: " + name);
  if (!isSourceName(source, circuit.gates))
    throw new Error("unknown output source: " + source);
  // Outputs are sinks (nothing reads them), so a gate -> output wire can
  // never create a cycle; the check lives in setGateInput alone.
  circuit.outputs[name] = source;
}

// Structural check: right shape, real gate types, right input counts,
// every source resolvable, no cycles. Throws with a message naming the
// first offender. (Pixel-range and eval-time checks stay with the engine.)
function validate(circuit) {
  const { GATES, OUTPUTS } = requireGates();
  if (!circuit || typeof circuit !== "object")
    throw new Error("circuit must be an object");
  const gs = circuit.gates;
  if (!gs || typeof gs !== "object") throw new Error("circuit has no gates object");
  for (const [id, g] of Object.entries(gs)) {
    if (!g || !GATES[g.type])
      throw new Error("gate " + id + ": unknown type " + (g && g.type));
    const n = GATES[g.type].inputs;
    if (!Array.isArray(g.in) || g.in.length !== n)
      throw new Error("gate " + id + " (" + g.type + ") needs " + n +
                      " input(s), got " + (Array.isArray(g.in) ? g.in.length : 0));
    for (const s of g.in)
      if (!isSourceName(s, gs)) throw new Error("gate " + id + ": unknown input source: " + s);
  }
  if (!circuit.outputs || typeof circuit.outputs !== "object")
    throw new Error("circuit has no outputs");
  for (const k of OUTPUTS)
    if (!(k in circuit.outputs)) throw new Error("circuit is missing output: " + k);
  for (const [k, s] of Object.entries(circuit.outputs))
    if (!isSourceName(s, gs)) throw new Error("output " + k + ": unknown source: " + s);
  // Cycles: a gate that appears in its own transitive fan-in.
  for (const id of Object.keys(gs))
    if (inFanIn(id, id, gs, new Set())) throw new Error("circuit cycle at gate " + id);
  return circuit;
}

// Serialize (for the editor's SAVE / the textarea / future persistence).
function toJSON(circuit) {
  validate(circuit);
  return JSON.stringify(circuit, null, 2);
}

// The inverse: parse + validate + a fresh copy (the parsed object is
// returned as-is, ready to be the live circuit).
function fromJSON(text) {
  let c;
  try { c = JSON.parse(text); }
  catch (e) { throw new Error("circuit JSON is not valid JSON: " + e.message); }
  validate(c);
  return c;
}

if (typeof module !== "undefined")
  module.exports = {
    emptyCircuit, addGate, deleteGate, setGateInput, setOutput,
    validate, toJSON, fromJSON, isSourceName,
  };
if (typeof globalThis !== "undefined") {
  globalThis.Racetrack = globalThis.Racetrack || {};
  globalThis.Racetrack.Circuit = {
    emptyCircuit, addGate, deleteGate, setGateInput, setOutput,
    validate, toJSON, fromJSON, isSourceName,
  };
}
