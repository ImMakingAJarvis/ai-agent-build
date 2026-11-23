---
id: sim-racetrack
name: Race-car sim (RAC)
summary: The RAC sim's static page — a top-down B/W race track with an arrow-key car, served on :8200.
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

First piece of the race-car simulator (RAC, `docs/sim-racetrack.md`): a
top-down black-and-white race track with a car you drive around using the
arrow keys. Plain JS, no build step, no framework — strictly two colors
(black background, white road; the car is black on the road). Later RAC
jobs add the fake camera (RAC-3), the logic-gate engine (RAC-4), the
circuit editor (RAC-5) and the demo circuit (RAC-6) into the same
directory.

## Interfaces

- `http://111.1.2.27:8200/` — the page (LAN address). Serves
  `index.html`, `sim.js` (the pure track/car core) and `page.js`
  (canvas + keys) from the image.
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

## Failure modes

- None recorded yet. The page is stateless; a bad image is fixed by
  `docker compose build sim-racetrack && docker compose up -d
  sim-racetrack`.
