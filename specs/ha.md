---
id: ha
name: HA sensor publisher (jarvis-ha)
summary: Publishes Jarvis headline sensors (queue depth, current job, loaded model, API spend, GPU temp/power) to Home Assistant over MQTT.
group: house
kind: container
status: active
compose: [ha]
units: [jarvis-ha.service]
binaries: [cmd/jarvis-ha]
ports: ["— (MQTT out)"]
docs: [docs/system-frame.md]
depends_on:
  - to: rabbitmq
    why: "MQTT 1883 (the rabbitmq_mqtt plugin) and queue depths"
  - to: engine
    why: "loaded-model sensor (HA_ENGINE_URL)"
  - to: jarvis-jobs
    why: "current-job sensor from the job store"
  - to: home-assistant
    why: "auto-discovered \"Jarvis Agent\" device"
---

## Purpose

Phase 7: MQTT auto-discovery sensors on the Home Assistant box, with a
last-will availability topic.

## Interfaces

- Publishes discovery + state topics on MQTT `rabbitmq:1883`.

## Configuration

- `HA_MQTT_URL`, `HA_ENGINE_URL`, `RABBITMQ_URL`, `JARVIS_CONF`;
  `env_file`: `agent.env`, `ha.env`. nvidia-smi bind-mounted for GPU
  sensors. `systemd/jarvis-ha.service` is the legacy host unit (disabled).

## Data

- None of its own.

## Deploy

    docker compose build ha && docker compose up -d ha

## Verify live

    docker logs --tail 20 jarvis-ha

Then check the Jarvis Agent device's sensors in Home Assistant.

## Failure modes

- Crash-looped while the broker could not boot (2026-11-04).
