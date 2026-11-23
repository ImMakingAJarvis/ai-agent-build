// editor.js — the RAC circuit editor UI (docs/sim-racetrack.md, RAC-5).
//
// A black-and-white panel below the track with three blocks:
//   INPUTS   the live 16x8 camera grid — every pixel a tappable source
//            (cam.r<row>.c<col>), plus the const.1 / const.0 cells.
//   GATES    the working area: pick a gate type in the toolbar, click to
//            place it; click a gate's input pin then a source to wire it;
//            click a gate body to select, DEL button (or right-click) to
//            delete; the circuit redraws on every change.
//   OUTPUTS  throttle / steerLeft / steerRight, each showing its live 0/1.
//
// Live signals: once per frame the editor asks the RAC-4 engine
// (Gates.values) for every gate's output from the live camera grid, and
// wires/gates light up white while they carry a 1. The editor never
// reimplements gate logic — it only mutates the circuit model
// (circuit.js) and reads the engine's answers.
//
// Modes: MANUAL (the arrow keys drive, as before) and AUTO (each frame the
// car's input is replaced by step(circuit, camera.grid()), in place of the
// keys). RESET puts the car back on the start line. SAVE / LOAD move the
// circuit in and out of the JSON textarea, so a good circuit can be kept.

"use strict";

function makeEditor(opts) {
  const { canvas, jsonArea, page, camera,
          gateButtons, deleteButton, manualButton, autoButton,
          resetButton, saveButton, loadButton } = opts;
  const { Gates } = globalThis.Racetrack;
  const Circuit = globalThis.Racetrack.Circuit;
  const ctx = canvas.getContext("2d");

  // --- layout (canvas coordinates) -------------------------------------
  const L = {
    W: 960, H: 460,
    cell: 18,                       // input-block pixel size
    gridX: 16, gridY: 48,           // 16 cols x 8 rows of cells
    constY: 206, constH: 26,        // const.1 / const.0 cells
    constX: [16, 164], constW: 140,
    gatesX: 330, gatesY: 36, gatesW: 390, gatesH: 408,
    outX: 748, outW: 196, outH: 96,
    outSlots: [
      { name: "throttle",   y: 48 },
      { name: "steerLeft",  y: 168 },
      { name: "steerRight", y: 288 },
    ],
    gateW: 88, gateH: 44,
  };
  canvas.width = L.W;
  canvas.height = L.H;

  // --- state -------------------------------------------------------------
  let circuit = Circuit.emptyCircuit();
  let mode = "MANUAL";
  let selected = null;      // a gate id (for DEL)
  let tool = null;          // { kind: "place", type } while a gate type is armed
  let armed = null;         // an in-progress wire:
                            //   { kind: "src", source }           from a pixel/const/gate
                            //   { kind: "pin", gate, slot }       from a gate's input pin
                            //   { kind: "out", name }             from an output slot
  let live = null;          // last Gates.values() result (or null on error)
  let liveErr = null;       // last engine error (wiring the car stops)
  let opErr = null;         // last model error (load/delete/wire), shown once
  const ZERO = { throttle: 0, brake: 0, steerLeft: 0, steerRight: 0 };

  // --- geometry helpers ----------------------------------------------------
  const cellRect = (r, c) => ({ x: L.gridX + c * L.cell, y: L.gridY + r * L.cell });
  const constRect = (i) => ({ x: L.constX[i], y: L.constY, w: L.constW, h: L.constH });
  const pinPos = (g, slot) => ({ x: g.x - 5, y: g.y + 14 + slot * 16 });
  const outAnchorOf = (name) => {
    const s = L.outSlots.find((o) => o.name === name);
    return { x: L.outX, y: s.y + L.outH / 2 };
  };
  const gateOutAnchor = (g) => ({ x: g.x + L.gateW, y: g.y + L.gateH / 2 });

  function sourceAnchor(source, grid) {
    const m = /^cam\.r(\d+)\.c(\d+)$/.exec(source);
    if (m) {
      const p = cellRect(+m[1], +m[2]);
      return { x: p.x + L.cell, y: p.y + L.cell / 2 };
    }
    if (source === "const.1" || source === "const.0") {
      const r = constRect(source === "const.1" ? 0 : 1);
      return { x: r.x + r.w, y: r.y + r.h / 2 };
    }
    const g = circuit.gates[source];
    return g ? gateOutAnchor(g) : null;
  }

  // Hit-test a canvas point. Returns one of:
  //   {kind:"cell",r,c} {kind:"const",name} {kind:"pin",id,slot}
  //   {kind:"gate",id} {kind:"out",name} {kind:"area"} | null
  function hitTest(x, y) {
    const gw = camera.cols * L.cell, gh = camera.rows * L.cell;
    if (x >= L.gridX && x < L.gridX + gw && y >= L.gridY && y < L.gridY + gh)
      return { kind: "cell", r: Math.floor((y - L.gridY) / L.cell),
               c: Math.floor((x - L.gridX) / L.cell) };
    for (let i = 0; i < 2; i++) {
      const r = constRect(i);
      if (x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + r.h)
        return { kind: "const", name: i === 0 ? "const.1" : "const.0" };
    }
    // Pins first (they sit on the gate's left edge).
    for (const [id, g] of Object.entries(circuit.gates))
      for (let s = 0; s < g.in.length; s++) {
        const p = pinPos(g, s);
        if (Math.hypot(x - p.x, y - p.y) <= 8)
          return { kind: "pin", id, slot: s };
      }
    for (const [id, g] of Object.entries(circuit.gates))
      if (x >= g.x && x < g.x + L.gateW && y >= g.y && y < g.y + L.gateH)
        return { kind: "gate", id };
    for (const s of L.outSlots)
      if (x >= L.outX && x < L.outX + L.outW && y >= s.y && y < s.y + L.outH)
        return { kind: "out", name: s.name };
    if (x >= L.gatesX && x < L.gatesX + L.gatesW &&
        y >= L.gatesY && y < L.gatesY + L.gatesH)
      return { kind: "area" };
    return null;
  }

  // The named source a hit refers to (for starting a wire): a pixel, a
  // constant, or a gate (its output). null otherwise.
  function sourceOf(hit) {
    if (!hit) return null;
    if (hit.kind === "cell") return "cam.r" + hit.r + ".c" + hit.c;
    if (hit.kind === "const") return hit.name;
    if (hit.kind === "gate") return hit.id;
    return null;
  }

  // --- editing -------------------------------------------------------------
  function deleteGate(id) {
    try {
      Circuit.deleteGate(circuit, id);
      if (selected === id) selected = null;
      opErr = null;
    } catch (e) { opErr = e.message; }
  }

  function tryConnect(hit) {
    if (!hit) return false;
    let fn, args;
    if (armed.kind === "src") {
      if (hit.kind === "pin") { fn = Circuit.setGateInput; args = [circuit, hit.id, hit.slot, armed.source]; }
      else if (hit.kind === "out") { fn = Circuit.setOutput; args = [circuit, hit.name, armed.source]; }
      else return false;
    } else {
      const s = sourceOf(hit);
      if (!s) return false;
      if (armed.kind === "pin") { fn = Circuit.setGateInput; args = [circuit, armed.gate, armed.slot, s]; }
      else if (armed.kind === "out") { fn = Circuit.setOutput; args = [circuit, armed.name, s]; }
      else return false;
    }
    try { fn(...args); opErr = null; return true; }
    catch (e) { opErr = e.message; armed = null; return true; }
  }

  function handleClick(x, y) {
    const hit = hitTest(x, y);
    // Placing a gate: the toolbar type is armed, click in the gates area.
    if (tool) {
      if (hit && (hit.kind === "area" || hit.kind === "gate")) {
        try {
          const gx = Math.max(L.gatesX + 4, Math.min(L.gatesX + L.gatesW - L.gateW - 4, x - L.gateW / 2));
          const gy = Math.max(L.gatesY + 4, Math.min(L.gatesY + L.gatesH - L.gateH - 4, y - L.gateH / 2));
          selected = Circuit.addGate(circuit, tool.type, gx, gy);
          opErr = null;
        } catch (e) { opErr = e.message; }
        tool = null;
        updateButtons();
      } else { tool = null; updateButtons(); }
      return;
    }
    // Finishing an in-progress wire.
    if (armed) {
      if (tryConnect(hit)) { armed = null; return; }
      // Not a target: re-arm from what was clicked (or cancel).
      const s = sourceOf(hit);
      if (s) { armed = { kind: "src", source: s }; return; }
      if (hit.kind === "pin") { armed = { kind: "pin", gate: hit.id, slot: hit.slot }; return; }
      if (hit.kind === "out") { armed = { kind: "out", name: hit.name }; return; }
      armed = null;
      return;
    }
    // Starting something.
    if (!hit) { selected = null; return; }
    const s = sourceOf(hit);
    if (s) { armed = { kind: "src", source: s }; return; }
    if (hit.kind === "pin") { armed = { kind: "pin", gate: hit.id, slot: hit.slot }; return; }
    if (hit.kind === "gate") { selected = (selected === hit.id ? null : hit.id); return; }
    if (hit.kind === "out") { armed = { kind: "out", name: hit.name }; return; }
    selected = null; // plain area click: deselect
  }

  function setMode(m) {
    mode = m;
    if (m === "MANUAL") page.setInput(ZERO); // don't let an AUTO value stick
    updateButtons();
  }

  // SAVE: the current circuit as JSON into the textarea (and back to the
  // caller). LOAD: parse + validate the textarea into a new live circuit.
  function save() {
    try {
      const text = Circuit.toJSON(circuit);
      jsonArea.value = text;
      opErr = null;
      return text;
    } catch (e) { opErr = e.message; return null; }
  }

  function load(text) {
    try {
      circuit = Circuit.fromJSON(text);
      selected = null; armed = null; opErr = null;
      return true;
    } catch (e) { opErr = e.message; return false; }
  }

  // --- buttons ---------------------------------------------------------------
  function updateButtons() {
    for (const b of gateButtons)
      b.className = (tool && tool.type === b.dataset.gate) ? "btn active" : "btn";
    manualButton.className = (mode === "MANUAL") ? "btn active" : "btn";
    autoButton.className = (mode === "AUTO") ? "btn active" : "btn";
  }

  for (const b of gateButtons)
    b.addEventListener("click", () => {
      tool = { kind: "place", type: b.dataset.gate };
      armed = null;
      updateButtons();
    });
  deleteButton.addEventListener("click", () => { if (selected) deleteGate(selected); });
  manualButton.addEventListener("click", () => setMode("MANUAL"));
  autoButton.addEventListener("click", () => setMode("AUTO"));
  resetButton.addEventListener("click", () => page.reset());
  saveButton.addEventListener("click", () => save());
  loadButton.addEventListener("click", () => load(jsonArea.value));

  canvas.addEventListener("click", (e) => handleClick(e.offsetX, e.offsetY));
  // Right-click a gate to delete it.
  canvas.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    const hit = hitTest(e.offsetX, e.offsetY);
    if (hit && hit.kind === "gate") deleteGate(hit.id);
  });

  // --- per-frame: live signals + AUTO input -------------------------------
  function preFrame(dt) {
    const grid = camera.grid();
    let v = null;
    try { v = Gates.values(circuit, grid); }
    catch (e) { liveErr = e.message; }
    live = v;
    if (v) liveErr = null;
    if (mode === "AUTO")
      page.setInput(v
        ? { throttle: v.throttle, brake: 0, steerLeft: v.steerLeft, steerRight: v.steerRight }
        : ZERO);
    draw(grid);
  }
  page.onFrame(preFrame);

  // --- drawing (strictly #000 / #fff) ---------------------------------------
  function wireValue(source, grid) {
    const m = /^cam\.r(\d+)\.c(\d+)$/.exec(source);
    if (m) return grid[+m[1]][+m[2]] ? 1 : 0;
    if (source === "const.1") return 1;
    if (source === "const.0") return 0;
    return (live && live[source]) ? 1 : 0;
  }

  // A wire: an elbow from (sx,sy) to (tx,ty). Carrying 1 -> thick solid;
  // carrying 0 -> thin dashed. Both white.
  function drawWire(sx, sy, tx, ty, val) {
    const mx = (sx + tx) / 2;
    ctx.strokeStyle = "#fff";
    ctx.lineWidth = val ? 4 : 1.5;
    ctx.setLineDash(val ? [] : [5, 4]);
    ctx.beginPath();
    ctx.moveTo(sx, sy);
    ctx.lineTo(mx, sy);
    ctx.lineTo(mx, ty);
    ctx.lineTo(tx, ty);
    ctx.stroke();
    ctx.setLineDash([]);
  }

  // A signal dot: filled when carrying 1, ring when 0.
  function drawDot(x, y, val) {
    ctx.beginPath();
    ctx.arc(x, y, 5, 0, Math.PI * 2);
    if (val) { ctx.fillStyle = "#fff"; ctx.fill(); }
    else { ctx.strokeStyle = "#fff"; ctx.lineWidth = 1.5; ctx.stroke(); }
  }

  function draw(grid) {
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, L.W, L.H);
    ctx.fillStyle = "#fff";
    ctx.font = "13px monospace";
    ctx.fillText("INPUTS — camera pixels (tap to wire)", L.gridX, 32);
    ctx.fillText("GATES", L.gatesX + 8, 30);
    ctx.fillText("OUTPUTS — car controls", L.outX, 32);

    // INPUT block: the live camera grid, one cell per pixel.
    for (let r = 0; r < camera.rows; r++)
      for (let c = 0; c < camera.cols; c++) {
        const p = cellRect(r, c);
        const on = grid[r][c] ? 1 : 0;
        ctx.fillStyle = on ? "#fff" : "#000";
        ctx.fillRect(p.x, p.y, L.cell - 1, L.cell - 1);
        const armedHere = armed && armed.kind === "src" &&
          armed.source === "cam.r" + r + ".c" + c;
        ctx.strokeStyle = "#fff";
        ctx.lineWidth = armedHere ? 3 : 1;
        ctx.strokeRect(p.x + 0.5, p.y + 0.5, L.cell - 2, L.cell - 2);
      }
    // The two constant sources.
    for (let i = 0; i < 2; i++) {
      const r = constRect(i);
      const name = i === 0 ? "const.1" : "const.0";
      const armedHere = armed && armed.kind === "src" && armed.source === name;
      ctx.fillStyle = "#000";
      ctx.fillRect(r.x, r.y, r.w, r.h);
      ctx.strokeStyle = "#fff";
      ctx.lineWidth = armedHere ? 3 : 1;
      ctx.strokeRect(r.x + 0.5, r.y + 0.5, r.w - 1, r.h - 1);
      ctx.fillStyle = "#fff";
      ctx.font = "12px monospace";
      ctx.fillText(name, r.x + 8, r.y + 17);
    }

    // GATES area border + status line inside it.
    ctx.strokeStyle = "#fff";
    ctx.lineWidth = 1;
    ctx.strokeRect(L.gatesX + 0.5, L.gatesY + 0.5, L.gatesW - 1, L.gatesH - 1);

    // Wires (under the boxes): every gate input, then every output.
    for (const [id, g] of Object.entries(circuit.gates)) {
      for (let s = 0; s < g.in.length; s++) {
        const a = sourceAnchor(g.in[s], grid);
        if (!a) continue;
        const p = pinPos(g, s);
        drawWire(a.x, a.y, p.x, p.y, wireValue(g.in[s], grid));
      }
    }
    for (const o of L.outSlots) {
      const src = circuit.outputs[o.name];
      const a = sourceAnchor(src, grid);
      if (!a) continue;
      const t = outAnchorOf(o.name);
      drawWire(a.x, a.y, t.x, t.y, wireValue(src, grid));
    }

    // Gate boxes.
    for (const [id, g] of Object.entries(circuit.gates)) {
      const sel = selected === id;
      ctx.lineWidth = sel ? 3 : 2;
      ctx.strokeStyle = "#fff";
      if (sel) { ctx.fillStyle = "#fff"; ctx.fillRect(g.x, g.y, L.gateW, L.gateH); }
      ctx.strokeRect(g.x, g.y, L.gateW, L.gateH);
      ctx.fillStyle = sel ? "#000" : "#fff";
      ctx.font = "10px monospace";
      ctx.fillText(id, g.x + 5, g.y + 13);
      ctx.font = "15px monospace";
      ctx.fillText(g.type, g.x + (L.gateW - g.type.length * 9) / 2, g.y + 30);
      // Pins (left) and output dot (right): filled while carrying 1.
      for (let s = 0; s < g.in.length; s++) {
        const p = pinPos(g, s);
        drawDot(p.x, p.y, wireValue(g.in[s], grid));
      }
      const oa = gateOutAnchor(g);
      drawDot(oa.x, oa.y, live && live[id] ? 1 : 0);
    }

    // OUTPUT block: the three car controls with live 0/1.
    for (const o of L.outSlots) {
      ctx.strokeStyle = "#fff";
      ctx.lineWidth = (armed && armed.kind === "out" && armed.name === o.name) ? 3 : 2;
      ctx.strokeRect(L.outX, o.y, L.outW, L.outH);
      ctx.fillStyle = "#fff";
      ctx.font = "14px monospace";
      ctx.fillText(o.name, L.outX + 10, o.y + 24);
      ctx.font = "11px monospace";
      ctx.fillText("src: " + circuit.outputs[o.name], L.outX + 10, o.y + L.outH - 12);
      ctx.font = "30px monospace";
      ctx.fillText(live ? String(live[o.name]) : "-", L.outX + L.outW - 44, o.y + 52);
    }

    // Bottom status line: mode, what is armed, and any error (white text).
    let status = "MODE: " + mode;
    if (tool) status += "  |  click in the gates area to place a " + tool.type;
    else if (armed) {
      if (armed.kind === "src") status += "  |  wire " + armed.source + " -> click a pin or output";
      else if (armed.kind === "pin") status += "  |  wire " + armed.gate + "." + armed.slot + " -> click a source";
      else status += "  |  wire " + armed.name + " <- click a source";
    }
    if (selected) status += "  |  selected: " + selected + " (DEL to remove)";
    const err = opErr || liveErr;
    if (err) status += "  |  ERROR: " + err;
    ctx.fillStyle = "#fff";
    ctx.font = "12px monospace";
    ctx.fillText(status.slice(0, 150), 16, L.H - 10);
  }

  draw(camera.grid()); // first paint before the rAF loop takes over

  return {
    canvas,
    get circuit() { return circuit; },
    get mode() { return mode; },
    setMode,
    save,
    load,
    click: handleClick,
    draw,
    deleteGate,
  };
}

if (typeof module !== "undefined") module.exports = { makeEditor };

// Browser bootstrap: the page (page.js) and the camera already exist on
// window; the editor builds its panel from the elements in index.html.
if (typeof document !== "undefined" && typeof window !== "undefined") {
  const editor = makeEditor({
    canvas: document.getElementById("circuit"),
    jsonArea: document.getElementById("circuit-json"),
    page: window.__racPage,
    camera: window.__racCamera,
    gateButtons: Array.from(document.querySelectorAll(".gate-tool")),
    deleteButton: document.getElementById("editor-delete"),
    manualButton: document.getElementById("mode-manual"),
    autoButton: document.getElementById("mode-auto"),
    resetButton: document.getElementById("sim-reset"),
    saveButton: document.getElementById("circuit-save"),
    loadButton: document.getElementById("circuit-load"),
  });
  window.__racEditor = editor;
}
