# Jarvis HomeAI — Home Assistant Integration

Monitors the Jarvis AI agent system and provides voice announcements when jobs complete.

## Features

- **Voice Announcements**: When a build job finishes, Jarvis checks if you're home and announces completion over all speakers (configurable via toggle switch).
- **HomeAI Dashboard**: Lovelace dashboard showing GPU metrics, system status, job stats, and controls.
- **Toggle Switch**: `input_boolean.job_announce` to enable/disable voice announcements.

## Architecture

```
[Job completes in agent-run]
        │
        ▼
[jarvis-homeai announce "message"]
        │
   ┌────┴────┐
   │ Check:  │
   │ 1. Switch on?
   │ 2. Owner home?
   └────┬────┘
        │ (both yes)
        ▼
[TTS to all media_player entities]
```

## Installation

### 1. Install jarvis-homeai binary

```bash
cd /home/agent/ai-agent-build
go build -o /usr/local/bin/jarvis-homeai ./cmd/jarvis-homeai/
```

### 2. Home Assistant Configuration

Add to your `configuration.yaml`:

```yaml
# Toggle switch for voice announcements
input_boolean:
  job_announce:
    name: "Jarvis Job Announcements"
    initial: on
    icon: "mdi:volume-source"

# Sensors for the dashboard
sensor:
  - platform: command_line
    name: "Jarvis GPU Memory Used"
    command: "nvidia-smi --query-gpu=memory.used --format=csv,noheader 2>/dev/null || echo 0"
    scan_interval: 15
    unit_of_measurement: "MiB"

  - platform: command_line
    name: "Jarvis GPU Temperature"
    command: "nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader 2>/dev/null || echo 0"
    scan_interval: 30
    unit_of_measurement: "°C"
    device_class: "temperature"

  - platform: command_line
    name: "Jarvis GPU Utilization"
    command: "nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader 2>/dev/null || echo 0"
    scan_interval: 15
    unit_of_measurement: "%"

  - platform: command_line
    name: "Jarvis GPU Power Draw"
    command: "nvidia-smi --query-gpu=power.draw --format=csv,noheader 2>/dev/null || echo 0"
    scan_interval: 15
    unit_of_measurement: "W"
    device_class: "power"

  - platform: command_line
    name: "Jarvis Current Job"
    command: "cat /home/agent/.config/jarvis/current-job 2>/dev/null || echo 'idle'"
    scan_interval: 10

  - platform: command_line
    name: "Jarvis Engine Status"
    command: "systemctl is-active jarvis-engine.service 2>/dev/null || echo 'inactive'"
    scan_interval: 30

  - platform: command_line
    name: "Jarvis Worker Status"
    command: "systemctl is-active worker.service 2>/dev/null || echo 'inactive'"
    scan_interval: 30

  - platform: command_line
    name: "Jarvis Chat Status"
    command: "systemctl is-active jarvis-chat 2>/dev/null || echo 'inactive'"
    scan_interval: 30
```

See `configuration.yaml` in this directory for the full config including Prometheus REST sensors.

### 3. Install the Dashboard

Copy `lovelace_dashboard.yaml` to your HA config and add to `configuration.yaml`:

```yaml
lovelace:
  dashboards:
    jarvis_homeai:
      title: "Jarvis AI"
      icon: "mdi:robot"
      mode: yaml
      filename: jarvis_homeai.yaml
```

### 4. Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HA_URL` | `http://homeassistant.local:8123` | Home Assistant base URL |
| `HA_TOKEN` | *(required)* | Long-lived access token |
| `HA_PERSON_ENTITY` | `person.owner` | Person entity for presence check |

## CLI Usage

```bash
# Check if announcement switch is on
jarvis-homeai switch get

# Toggle announcements on/off
jarvis-homeai switch on
jarvis-homeai switch off

# Check if owner is home
jarvis-homeai presence

# List media players
jarvis-homeai players

# Send TTS directly (bypasses switch/presence checks)
jarvis-homeai tts "Hello from Jarvis"

# Full announce flow (checks switch + presence, then TTS)
jarvis-homeai announce "I finished the build"
```

## Dashboard Layout

The dashboard has 3 views:

1. **Overview**: System status, controls (announcement toggle), GPU gauges, VRAM, job stats
2. **GPU**: Detailed GPU monitoring with gauges and histograms
3. **Jobs**: Job statistics and history graphs

The announcement toggle switch (`input_boolean.job_announce`) appears on the Overview dashboard under "Controls".
