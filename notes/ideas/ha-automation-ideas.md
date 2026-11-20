# ha-automation-ideas — five automations: laundry handoff, arrival lights, Jarvis-down alarm, GPU-heat coordination, "Jarvis is working" light

2026-11-20, think job (ha-automation-ideas). Grounded in a live pull of HA:
~35 dimmable Hue lights (Foyer, Staircase, Kitchen, Living Room, Dining,
Main Bedroom, Princess Den, Sun Office, Upstairs), 7 GE/SmartHQ mini-splits
(one per room: Living Room, Main Bedroom, Princess Den, Bay Window Room,
Art Room, Kitchen Annex Storage Room, Upper Front), washer + dryer with
per-cycle SmartHQ alert sensors, and motion at Front Door, Back Door
(Kitchen), Living Room, Art Room, Mom's Doorbell, and Upper Deck.

## What I found

- **The `jarvis_*` sensors are NOT in HA right now.** `jarvis-ha` (the
  Phase 7 MQTT publisher) is inactive — last log line 2026-10-27
  "shutting down" — and HA has no entity matching "jarvis", "queue" or
  "gpu". Every sensor-based automation below therefore has one shared
  prerequisite: restart the publisher and confirm the MQTT integration is
  set up in HA (Phase 7 setup step 1, ha/README.md). This matches the
  power-economics note's finding that the documented HA power sensors
  don't exist.
- **The house's climate is the 7 SmartHQ mini-splits.** The "Silver
  Monkey Hvac" climate entity is the camper's unit (SolCamper area, doors/
  windows/locks all unavailable). Each split and both laundry machines
  expose SmartHQ problem sensors (Critical, Ventblocked, Endofcycle, …).
- **Laundry is the natural SmartHQ automation spot.** Washer + dryer
  online, HA has first-class SmartHQ control and completion-automation
  tooling. Caveat: the "Endofcycle" binary sensors are currently
  `unavailable` — likely they only exist while a cycle runs — so the
  trigger entity needs a 10-minute live check before wiring.
- **The box is the heater** (docs/idle-work.md): ~130 W idle, 200–350 W
  working (power-economics note). That heat is the hook for idea 4.
- **Staircase night lighting is blocked**: no motion sensor in the
  Staircase, and the "Foyer Stairs" light entity is `unavailable`.
  Fix the entity / add a sensor before building that variant.

## The five automations

### 1. Washer → dryer handoff (house-native, no prerequisite)
**Trigger:** washer end-of-cycle — SmartHQ completion alert (or
`binary_sensor.washer_alert_endofcycle` once its state behaviour is
verified live).
**Action:** start the Dryer via SmartHQ; on dryer end-of-cycle → Discord
notify Nathan and turn on the Laundry light.
**Why:** a forgotten dryer is the one laundry failure with a fire risk;
the handoff kills the "move the clothes" chore entirely.
**Check first:** Endofcycle sensor states; fallback is SmartHQ's native
completion-automation webhook. Fold in a "Ventblocked" → immediate
notify while there.

### 2. Night arrival lights (house-native, no prerequisite)
**Trigger:** Front Door Motion or Back Door Motion (Kitchen) between
sunset and sunrise while Foyer/Downstairs lights are off.
**Action:** Foyer Door, Foyer Hallway and Downstairs at 30% for 5 min.
**Why:** classic, zero new hardware, and both motion sensors are live
today. Same path can serve Mom's Doorbell at night.

### 3. Jarvis-down alarm from HA (needs jarvis-ha up)
**Trigger:** the Jarvis Agent device — e.g. `sensor.jarvis_current_job` —
unavailable for ≥ 2 min. The publisher's MQTT last-will fires exactly
when the publisher or the whole box dies.
**Action:** Discord notify Nathan + a distinct light cue (Downstairs or
Staircase).
**Why:** the stack's own watchdog dies with the box; HA is the second
always-on system in the house and the only one positioned to announce
"the box is dead". This is what the Phase 7 sensors were actually for.

### 4. GPU heat ↔ room AC coordination (needs jarvis-ha up)
**Trigger (cool season):** `sensor.jarvis_gpu_power` > 300 W for 15+ min
AND the box's room above setpoint → turn on that room's mini-split.
**Trigger (heat season):** box idle at night (`sensor.jarvis_current_job`
= `idle` / a think job) and room below setpoint → keep the split off and
let the ~130 W of idle heat earn its keep.
**Why:** the box doubles as the heater for this place (idle-work.md);
this turns its power draw into a thermostat input instead of fighting
the AC. **Open:** confirm which room the box lives in (Sun Office?
basement?).

### 5. "Jarvis is working" ambient light (needs jarvis-ha up)
**Trigger:** `sensor.jarvis_current_job` changes `idle` → any job.
**Action:** a Hue accent — Side gradient or Hue Play wall washers in the
Main Bedroom, or Sun Area in the Sun Office — to soft blue at 5–10%;
back off when idle.
**Why:** the family can see at a glance that the agent is awake, and it
doubles as a canary that proves the MQTT sensor path works — a cheap
dress rehearsal for ideas 3 and 4.

## Recommendation

Order of work: (1) restart `jarvis-ha` and verify HA's MQTT integration —
unblocks 3–5, restores the Phase 7 dashboard card, ~15 min; (2) idea 2,
then idea 1 — no prerequisites, immediate value; (3) idea 3 — highest
value per line of config; (4) idea 4, cool-season version first; (5) idea
5 last, it's a toy.

If Nathan picks exactly one: **idea 3, the Jarvis-down alarm.** It is the
only automation on this list that protects the stack itself, and the
sensors it needs are already built and documented — just not running.
