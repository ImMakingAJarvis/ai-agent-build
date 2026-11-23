---
id: sim-racetrack
name: Race-car sim (RAC)
summary: The RAC sim's static page — a top-down B/W race track with an arrow-key car, a fake-camera "CAM" panel, and a logic-gate circuit editor with an AUTO mode, served on :8200.
group: face
kind: container
status: active
compose: [sim-racetrack]
units: []
binaries: []
ports: ["8200 (LAN)"]
health: http://sim-racetrack:80/
docs: [docs/sim-racetrack.md]
depends_on: []
---

## Purpose

Piece one and two of the race-car simulator (RAC,
`docs/sim-racetrack.md`): a top-down black-and-white race track with a
car you drive around using the arrow keys, plus a fake-camera "CAM" panel
that shows the 16x8 B/W pixel grid of the whole circuit from directly
above — a top-down view, framed on the track's bounding box, with the
car as a black dot (RAC-3).
Plain JS, no build step, no framework — strictly two colors (black
background, white road; the car is black on the road). The logic-gate
engine (RAC-4, `gates.js`) is wired in by the RAC-5 circuit editor: the
page shows the 16x8 camera grid as tappable inputs, a gate area, and the
three car-control outputs; a MANUAL/AUTO toggle makes the car obey the
circuit's `step()` each frame instead of the arrow keys. RAC-6 (the demo
circuit) remains.

## Interfaces

- `http://111.1.2.27:8200/` — the page (LAN address). Serves
  `index.html`, `sim.js` (the pure track/car core), `camera.js` (the
  pure fake-camera core), `gates.js` (the pure logic-gate circuit
  engine, RAC-4), `circuit.js` (the pure circuit model, RAC-5),
  `page.js` (canvas + keys + the CAM panel) and `editor.js` (the
  circuit editor panel, RAC-5) from the image.
- The fake camera (RAC-3) is a pure function of `{car, track}`:
  `camera.grid() -> number[][]` (16 cols x 8 rows, `grid[row][col]`,
  row 0 = top of the view, col 0 = leftmost, `1 = road / 0 = wall or
  the car`) and `camera.pixel(row, col) -> 0|1`. The view is a
  fixed top-down look at the whole circuit, framed on
  `Track.bounds()` — the circuit determines where the camera is; the
  road stays put and the car is a black dot at the sample point nearest
  its center, so the panel updates as the car drives. RAC-4 (the
  logic-gate engine) consumes `grid()`/`pixel()`.
- The logic-gate engine (RAC-4, `gates.js`) is a pure function, headless
  (no DOM): `Gates.step(circuit, cameraGrid) -> { throttle, steerLeft,
  steerRight }` (each 0/1), topological evaluation of a gate graph.
  Gate set: NOT (1-input), AND, OR, NAND, NOR, XOR, XNOR (2-input).
  Circuit = `{ gates: { id: { type, in: [source, ...] } },
  outputs: { throttle, steerLeft, steerRight } }`; a source is a gate
  id, a camera pixel `cam.r<row>.c<col>` (the RAC-3 grid), or a
  constant `const.1`/`const.0`. `Gates.values(circuit, cameraGrid)`
  additionally returns every gate's live output (for the RAC-5
  editor). Wired into the page by the RAC-5 editor: `Gates.values()`
  runs every frame, and in AUTO mode its outputs are the car's input.
- The circuit editor (RAC-5): `circuit.js` is the pure model —
  `emptyCircuit()` (no gates, all outputs `const.0`), `addGate`,
  `deleteGate` (dependents rewired to `const.0`), `setGateInput`,
  `setOutput`, `validate` (shape, arity, sources, cycles), `toJSON` /
  `fromJSON` (parse + validate). `editor.js` is the DOM panel: the
  16x8 camera grid as tappable named inputs (`cam.r<row>.c<col>`),
  `const.1`/`const.0` cells, a gate area with the seven gate types from
  a toolbar, the three outputs with live 0/1 indicators, and wires that
  draw white solid while carrying 1 and dashed while 0. Click a source
  then a pin or output to wire; DEL / right-click deletes a gate.
  SAVE / LOAD move the circuit to and from a JSON textarea. The mode
  toggle is MANUAL (arrow keys) / AUTO (the circuit's outputs replace
  the key input before every `Car.step`; switching back zeroes it), and
  RESET puts the car back on the start line. The editor never
  reimplements gate logic — it mutates the model and reads
  `Gates.values()` each frame; a circuit that stops evaluating shows
  its error in the panel and the car parks.
- No API, no state, no other services.

## Configuration

- None. No env vars, no files; the image is the static content. The
  physics tuning (speeds, steering, track size) lives at the top of
  `sim/racetrack/sim.js`.

## Data

- Nothing persistent. The container is stateless; `docker rm` is safe.

## Deploy

    docker compose build sim-racetrack && docker compose up -d sim-racetrack

Confirm the running thing is the new one: `curl -s
http://111.1.2.27:8200/sim.js | head -3` shows the current file head.

## Verify live

- `curl -s http://111.1.2.27:8200/ | head` returns the page.
- Open `http://111.1.2.27:8200/` in a browser: the track + car render,
  the arrow keys drive the car, and the car cannot leave the road.
- Headless physics (no browser on the box): `node
  sim/racetrack/sim.test.js` (full lap, on-road invariant, wall
  collision) and `node sim/racetrack/page.test.js` (the real key
  handlers drive the car through the fake DOM, plus the RAC-5 editor:
  AUTO with the empty circuit is a parked car; the acceptance circuit
  `throttle = cam.r6.c8` drives the car while that pixel sees road and
  releases when the car's dot covers it; MANUAL restores the keys;
  placing and wiring a gate through the editor's own click path;
  SAVE/LOAD through the textarea with bad-JSON refusal; cycle refusal;
  RESET).
- Headless circuit model (no browser): `node --test
  sim/racetrack/circuit.test.js` — gate placement/ids, input and
  output validation, self-loop and transitive-cycle refusal,
  deleteGate rewiring, toJSON/fromJSON round-trip through the real
  gates engine, and the fromJSON rejections (bad JSON, missing
  outputs, wrong arity, unknown source, hidden cycle).
- Headless camera: `node --test sim/racetrack/camera.test.js` — pins the
  grid convention (top-down map of the whole circuit: road ring with an
  empty infield, symmetric; the car is a black dot at the sample point
  nearest its center; moving the car moves the dot, the road and the
  heading do not change the pattern) and that `grid()`/`pixel()` agree
  and follow the car.
- The CAM panel's live update as the car drives is asserted in
  `page.test.js` (the grid is not deep-equal to the initial one after
  driving — the car's dot moved); the pixels themselves need a human
  browser.
- Headless gates (no browser): `node --test
  sim/racetrack/gates.test.js` — full truth table for every gate, a
  hand-built circuit with known `step()` outputs, outputs wired
  straight from pixels/constants, purity (no mutation of circuit or
  grid), `cam.r<row>.c<col>` names against the real RAC-3 grid, and
  the wiring errors (cycle, unknown gate type, wrong input count,
  unknown source, pixel out of grid, missing output).

## Failure modes

- None recorded yet. The page is stateless; a bad image is fixed by
  `docker compose build sim-racetrack && docker compose up -d
  sim-racetrack`.
