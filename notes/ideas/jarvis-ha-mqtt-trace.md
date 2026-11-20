# jarvis-ha-mqtt-trace — Why no sensor.jarvis_* exists in HA: no MQTT integration, discovery that never reaches wildcard subscribers, and swapped power values

**TL;DR** — The publisher (`jarvis-ha` container) is healthy and has been publishing all
eleven sensors to the broker every 30 s since 2026-11-05. Nothing exists in Home
Assistant because **HA has no MQTT integration configured** — the one-time
"setup in Home Assistant" step from `ha/README.md` was never done. Doing that
step alone still won't make the entities appear, because the RabbitMQ MQTT
plugin does **not redeliver retained messages to wildcard subscriptions** and
HA discovery subscribes by wildcard — the publisher must re-publish discovery
*after* the integration exists (a container restart). On top of that there is a
real code bug: `cmd/jarvis-ha/main.go:97` swaps the `TodayPowerCost` return
values, so once the sensors do appear, "Power Cost Today" shows a kWh number
and "Energy Today" shows the cost (currently 0.000).

## Findings (verified live, 2026-11-20)

1. **HA has no MQTT integration.** WebSocket `config_entries/get` on HA
   (2026.11.2 at 111.1.2.200): 55 integrations, zero of domain `mqtt`;
   `/api/states` has zero `sensor.jarvis_*` (only the voice
   `conversation.jarvis_voice_*`). So *all* eleven sensors are missing, not
   just the two power ones. Root cause #1: the manual one-time setup step was
   never performed.

2. **The broker holds everything.** All eleven value topics are retained and
   live under `jarvis/sensor/*` (probed by raw MQTT subscribe from the host:
   `current_job` shows the running job, `loaded_model` = jarvis-brain.gguf,
   GPU 48 °C / 58.9 W, all four queues 0). All eleven discovery configs are
   retained under `homeassistant/sensor/<unique_id>/config` (probed on exact
   topics). The `jarvis-ha` container (compose service `ha`) has run since
   2026-11-04, first successful broker connect 2026-11-05 19:52, logs clean —
   healthy ticks log nothing, and there is no "discovery failed" line.

3. **Retained payloads never reach wildcard subscribers.** HA's MQTT discovery
   subscribes `homeassistant/+/+/config`. Live tick delivery to wildcards works
   (12 messages in 40 s), but retained-redelivery on a fresh wildcard
   subscription does not — 6/6 attempts timed out while an exact-topic
   subscribe to the same config returned instantly. The already-retained
   discovery configs therefore cannot be delivered to a newly added HA
   integration; discovery must be re-published *after* the integration exists.
   Root cause #2: startup-once `Discover()` (`cmd/jarvis-ha/main.go:48`) meets
   this plugin's retained/wildcard gap.

4. **The power values publish swapped (code bug).** `ha.TodayPowerCost`
   returns `(kwh, cost, err)` (`internal/ha/collect.go:252`), but the call
   site is `if cost, kwh, err := ha.TodayPowerCost(jobsLog)`
   (`cmd/jarvis-ha/main.go:97`) and assigns `s.PowerCostToday, s.EnergyToday =
   cost, kwh` — so the kWh total lands in PowerCostToday and the cost total in
   EnergyToday. Verified against live data: the broker's
   `power_cost_today = 5.5930` exactly equals today's `jobs.jsonl` kWh sum
   (5.593), and `energy_today = 0.000` equals today's cost sum (0). The
   library is unit-tested (`ha_test.go`) but the main.go pairing is not, which
   is how the swap shipped.

5. **Cost is structurally 0 until a rate is set.** `internal/power`:
   `Rate() = envFloat("JARVIS_POWER_RATE", 0)`, `SystemWatts() =
   envFloat("JARVIS_POWER_SYSTEM_W", 0)` — neither is set in any env file
   (checked agent.env, ha.env, agent-run unit), so every per-job
   `power_cost` is 0 and dropped by `omitempty` (`internal/agentjob/store.go:186`).
   The kWh numbers are real (sampled per job); the dollar numbers cannot be.

## Options / next steps

- **A — Add the MQTT integration in HA (no code).** Settings → Devices &
  Services → Add Integration → **MQTT**: host `111.1.2.27`, port `1883`,
  username/password = `RABBITMQ_DEFAULT_USER` / `RABBITMQ_DEFAULT_PASS` from
  the compose env. *Then immediately* `docker compose restart ha` so
  `Discover()` re-publishes the eleven configs live (required by finding 3).
  Verify eleven `sensor.jarvis_*` entities under the *Jarvis Agent* device.
- **B — Fix the swap (small PR).** `cmd/jarvis-ha/main.go:97`: `kwh, cost,
  err := ha.TodayPowerCost(jobsLog)`; assign `s.EnergyToday, s.PowerCostToday =
  kwh, cost`. Add a regression test pinning the pairing. Rebuild: `docker
  compose build ha && docker compose up -d ha`. While at it, update
  `ha/README.md`'s stale systemd references (`systemctl status jarvis-ha`) —
  it is a compose service now. Do B before A when possible: one container
  restart serves both, and HA automations shouldn't be wired to lying values.
- **C — Self-healing discovery (small code change, follow-up).** Re-publish
  the discovery configs periodically (every tick is 11 tiny retained messages;
  or on reconnect / hourly) so a later HA re-add or broker data loss cannot
  strand the entities. Fixes the startup-once design against the
  plugin's retained-wildcard gap.
- **D — Set the power rate (config, separate).** Set `JARVIS_POWER_RATE`
  (USD/kWh) in the agent-run environment (needs Nathan's number) so per-job
  `power_cost` is non-zero and "Power Cost Today" is meaningful. Without it,
  the cost sensor shows 0.0000 even after B (energy would be correct).
- **E — (fallback only)** If the plugin's retained-wildcard behavior still
  blocks after A+B, front the HA-facing broker with a small standalone
  mosquitto container. Heavier; don't reach for it until A+B prove
  insufficient.

## Recommendation

Do **B then A in one round**: fix the swap, rebuild and restart the `ha`
container (which re-publishes discovery live), add the MQTT integration in HA,
and verify all eleven entities. That unblocks the ha-automation ideas
(`notes/ideas/ha-automation-ideas.md`) with correct values in one pass. Queue
**C** and **D** as small follow-ups — D needs the rate number. Nothing in this
note was changed; it is a trace only.
