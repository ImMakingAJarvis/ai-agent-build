# Jarvis headline sensors in Home Assistant

Phase 7 publishes a small set of headline sensors from the Jarvis build
machine to Home Assistant via **MQTT auto-discovery**. Job-level views
(queue details, approvals, cost breakdown) stay in the Phase 6 dashboard;
Grafana keeps the hardware dashboards. HA gets:

| Entity | Meaning |
| --- | --- |
| `sensor.jarvis_queue_voice` | messages waiting in the `voice` queue |
| `sensor.jarvis_queue_fix` | messages waiting in the `fix` queue |
| `sensor.jarvis_queue_coding` | messages waiting in the `coding` queue |
| `sensor.jarvis_queue_background` | messages waiting in the `background` queue |
| `sensor.jarvis_current_job` | title of the job the agent is running now (`idle` when none) |
| `sensor.jarvis_loaded_model` | model loaded in the local engine |
| `sensor.jarvis_api_spend_today` | $ spent on API-dispatched jobs started today (UTC) |
| `sensor.jarvis_power_cost_today` | $ electricity cost of local agent jobs started today (UTC) |
| `sensor.jarvis_energy_today` | kWh sampled from local agent jobs started today (UTC) |
| `sensor.jarvis_gpu_temp` | GPU temperature (°C) |
| `sensor.jarvis_gpu_power` | GPU power draw (W) |

All entities live under one device, **Jarvis Agent**, and go unavailable
automatically if the publisher or broker drops (last-will + availability
topic).

## How it works

```
jarvis-ha (systemd, on Jarvis box)
  ├─ collects every 30s:
  │    RabbitMQ  → queue depths (QueueInspect)
  │    agentjob  → current running job (~/.local/share/jarvis/jobs/)
  │    engine    → loaded model (:11434 /v1/models or Ollama /api/ps)
  │    jobs.jsonl→ today's API spend (actual cost, else estimate)
  │    nvidia-smi→ GPU temp / power
  └─ publishes retained state to the RabbitMQ MQTT plugin (port 1883)
        homeassistant/sensor/<unique_id>/config  (auto-discovery, once)
        jarvis/sensor/<key>                      (values, every tick)
        jarvis/availability                      (online / offline)

Home Assistant (MQTT integration) ← picks up entities automatically
```

The broker is the existing `jarvis-rabbitmq` container with the
`rabbitmq_mqtt` plugin enabled — no new software. Port 1883 is exposed on
all interfaces in `docker-compose.yml` so the HA box on the LAN can
subscribe.

## One-time setup on the Jarvis machine

Already done by the phase install: `jarvis-ha` is built to
`/usr/local/bin/jarvis-ha`, the service is enabled, and the MQTT plugin
is on. Check health:

```bash
systemctl status jarvis-ha
tail -5 ~/.local/state/jarvis/ha-publisher.log
```

## One-time setup in Home Assistant

1. **Add the MQTT integration** — Settings → Devices & Services →
   Add Integration → **MQTT**:
   - Host: the Jarvis machine's LAN address (the one HA already uses for
     the voice service)
   - Port: `1883`
   - Username / Password: the RabbitMQ credentials
     (`RABBITMQ_DEFAULT_USER` / `RABBITMQ_DEFAULT_PASS` from the
     docker-compose env — the same user the AMQP traffic uses)
2. **Verify** — the eleven `sensor.jarvis_*` entities appear under the
   *Jarvis Agent* device. They update every ~30 s.
3. **Add the dashboard card** — `dashboard-snippet.yaml` in this folder
   adds one card: a **Jarvis Dashboard** button that opens the Phase 6
   dashboard (`http://<jarvis-host>:8080`) in a new tab, plus a glance of
   all headline sensors. Replace `<JARVIS_HOST>` before pasting.

## Tuning

Environment for `jarvis-ha` (override in the service unit or
`/home/agent/.config/jarvis/agent.env`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `HA_MQTT_URL` | `mqtt://localhost:1883` | broker URL |
| `HA_MQTT_USER` / `HA_MQTT_PASS` | – | broker credentials |
| `HA_TOPIC_PREFIX` | `jarvis` | value topic prefix |
| `HA_ENGINE_URL` | `http://localhost:11434` | model engine URL |
| `HA_PUBLISH_INTERVAL` | `30s` | publish cadence |
| `HA_GPU_CMD` | `nvidia-smi` | nvidia-smi path |

## Troubleshooting

- **Entities never appear in HA**: HA can't reach the broker. From the
  HA box: `mosquitto_pub -h <jarvis> -p 1883 -u jarvis -P <pass> -t test -m hi`
  (or check the MQTT integration's logs). Then check
  `docker ps` shows `jarvis-rabbitmq` and `ss -tlnp | grep 1883` on the
  Jarvis box.
- **Entities stuck "unavailable"**: the publisher is down —
  `systemctl status jarvis-ha`, log at
  `~/.local/state/jarvis/ha-publisher.log`.
- **Queue sensors missing a queue**: a queue that has never been declared
  in RabbitMQ is skipped (e.g. `fix` until a fix job is enqueued).
