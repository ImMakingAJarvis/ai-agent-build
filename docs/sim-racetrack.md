# Race-car simulator (JS) — plan

Nathan's idea (2026-11-21): a JavaScript simulator where a toy car drives
around a small race track. You drive it with the arrow keys. There is also a
**fake camera** that records the scene as a small black-and-white pixel grid;
that grid feeds a **circuit of logic gates** whose outputs drive the car.
The whole point: make a car go around a track with basic logic circuits.
Everything is **black and white** on purpose, to keep the circuit side easy.

Board: Plane project **RAC** (one project per epic, per the board standard).
Epic item `RAC-1`; tasks `RAC-2`…`RAC-6` (board refs, not plan numbers).
Live site target: `http://111.1.2.27:<port>/` (LAN address, never localhost).

## Interpretation note

"Analog circuit of logic gates" is read as a **visual, digital logic-gate
circuit** — a breadboard-style diagram of NOT/AND/OR/… gates wired together,
with live signals shown in black and white. It is digital logic (gates),
presented as a physical-looking circuit. If Nathan actually wanted analog
electronics (resistors/op-amps), that is a much larger project and this plan
would change — flagged in the Discord note so he can correct it.

## Architecture

- **Language:** plain JavaScript (no build step, no framework). Served as
  static files.
- **View:** a top-down 2D canvas of a closed-loop track (white road on black,
  or vice versa). The car is a small rectangle with a heading.
- **Car physics (2D, simple):** state = `{x, y, heading, speed}`.
  - `throttle` → speed increases (up to a max); friction decays it.
  - `brake/reverse` → speed decreases / goes negative (slow reverse).
  - `steerLeft`/`steerRight` → heading changes, scaled by speed (no turning
    while stopped).
  - The car **collides with the track edges**: if the next position is off the
    road, it does not move there (and speed is killed or reversed).
- **Input:** two modes.
  - **Manual:** arrow keys (up = throttle, down = brake/reverse, left/right =
    steer).
  - **Auto:** the logic circuit's outputs drive `throttle`, `steerLeft`,
    `steerRight` each frame.
- **Serve:** a new static compose service (e.g. `nginx:alpine`) serving the
  sim directory on a LAN port (start at **8200**; take the next free port if
  it is busy). A spec in `specs/<id>.md` is required (the specs test enforces
  one per compose service).

## Fake camera (RAC-3)

- Captures a small **black-and-white pixel grid** from the car's forward view.
  Default **16 cols × 8 rows** (configurable).
- **Coordinate convention (fixed, documented in code):**
  - `col 0` = leftmost … `col 15` = rightmost (across the field of view).
  - `row 0` = nearest to the car … `row 7` = farthest (depth).
  - Value `1` = road/track present, `0` = no road (wall/off-track).
- Rendered as a small "camera" panel on the page (blocky B/W pixels), updating
  live as the car moves/turns.
- **JS API for the circuit:**
  - `camera.grid()` → `number[][]`, `grid[row][col]`, 0/1.
  - `camera.pixel(row, col)` → 0/1.
- The camera is a pure function of `{car, track}` so it is unit-testable
  headless.

## Logic-gate engine (RAC-4)

- **Gate set (all 2-input unless noted):** `NOT`, `AND`, `OR`, `NAND`, `NOR`,
  `XOR`, `XNOR`. (2-input keeps the editor and the demo tractable.)
- A **circuit** is a graph: nodes = gates, edges = wires.
  - **Inputs:** named camera pixels, e.g. `cam.r0.c0`, plus optional manual /
    constant inputs.
  - **Outputs (fixed, the car controls):** `throttle`, `steerLeft`,
    `steerRight` — each is the output of exactly one gate (or a raw input).
- `step(circuit, cameraGrid) → { throttle, steerLeft, steerRight }` — a **pure
  function**, fully unit-testable with no browser/DOM.
- Evaluation is per-frame; the editor (RAC-5) shows each gate's live output.

## Circuit editor UI (RAC-5)

- A canvas/drag-and-drop editor on the page:
  - The **camera grid is the input block** (each pixel a tappable input).
  - A **gates area** where the user places gates, wires input→gate and
    gate→gate (and gate→output).
  - The **car controls are the output block** (throttle / steerLeft /
    steerRight), each showing live 0/1.
  - Live signal flow drawn in black and white (a wire lights when it carries 1).
  - Add / wire / delete gates. A mode toggle switches the car between Manual
    and Auto (circuit-driven).
- The editor persists the current circuit (in-page state; optional
  save/load-to-JSON).

## Demo + docs (RAC-6)

- A pre-wired **wall-follower** circuit that drives the car around the track
  on its own. Sketch (tuned by the builder in the harness):
  - Reduce the left and right halves of a front row into one bit each
    (OR-reduce a few pixels) → "road on the left?", "road on the right?".
  - `steerLeft  = roadOnRight AND NOT roadOnLeft` (track is to the right →
    turn back left is wrong; steer toward the open side).
  - `steerRight = roadOnLeft  AND NOT roadOnRight`.
  - `throttle   = NOT (NOT roadOnLeft AND NOT roadOnRight)` (brake if the road
    is gone on both sides ahead).
  - Exact pixel picks and wiring are tuned so the car completes laps without
  leaving the road.
- A **"load demo"** button installs this circuit into the editor.
- A `README` in the sim directory: how to drive manually, how the camera works
  (pixel convention), how to build a circuit, how the demo works.

## Jobs (each a separate PR; chained in the background queue)

| Plane | Job id | Scope | Depends on |
|-------|--------|-------|------------|
| RAC-2 | `sim-rac-track-car` | track + drivable car + static service + spec | — |
| RAC-3 | `sim-rac-camera` | fake camera B/W grid + panel + JS API | RAC-2 |
| RAC-4 | `sim-rac-gates` | headless logic-gate engine + unit tests | RAC-3 |
| RAC-5 | `sim-rac-editor` | circuit editor UI + live signals | RAC-4 |
| RAC-6 | `sim-rac-demo-docs` | wall-follower demo circuit + README | RAC-5 |

Each job: build + `go build/test` where relevant, **verify live on the LAN
site** (drive it / watch the camera / let the demo lap), open a PR, update the
matching Plane item, Discord-notify with the PR link. The car + track stay
strictly black and white throughout.

## Verify-live notes

- The sim is a browser app, so "live" means loading the page at the LAN
  address and exercising it (drive with arrows; watch the camera change; run
  the demo and watch laps). Where a headless check is possible (camera grid
  values, gate `step()` outputs), add unit tests so the logic is pinned down
  independent of the browser.
- The headless gate engine (RAC-4) and the camera function (RAC-3) are the two
  pieces with real unit-test value; the UI (RAC-2/5) is verified by driving it.
