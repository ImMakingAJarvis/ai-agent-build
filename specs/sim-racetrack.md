---
id: sim-racetrack
name: Race-car sim (RAC)
summary: The RAC sim's static page — a top-down B/W race track with an arrow-key car and a fake-camera "CAM" panel, served on :8200.
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
background, white road; the car is black on the road). Later RAC jobs add
the logic-gate engine (RAC-4, which consumes the camera's grid), the
circuit editor (RAC-5) and the demo circuit (RAC-6) into the same
directory.

## Interfaces

- `http://111.1.2.27:8200/` — the page (LAN address). Serves
  `index.html`, `sim.js` (the pure track/car core), `camera.js` (the
  pure fake-camera core) and `page.js` (canvas + keys + the CAM panel)
  from the image.
- The fake camera (RAC-3) is a pure function of `{car, track}`:
  `camera.grid() -> number[][]` (16 cols x 8 rows, `grid[row][col]`,
  row 0 = top of the view, col 0 = leftmost, `1 = road / 0 = wall or
  the car`) and `camera.pixel(row, col) -> 0|1`. The view is a
  fixed top-down look at the whole circuit, framed on
  `Track.bounds()` — the circuit determines where the camera is; the
  road stays put and the car is a black dot at the sample point nearest
  its center, so the panel updates as the car drives. RAC-4 (the
  logic-gate engine) consumes `grid()`/`pixel()`.
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
  handlers drive the car through the fake DOM).
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

## Failure modes

- None recorded yet. The page is stateless; a bad image is fixed by
  `docker compose build sim-racetrack && docker compose up -d
  sim-racetrack`.
