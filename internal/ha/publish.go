package ha

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Entity describes one auto-discovered Home Assistant sensor.
type Entity struct {
	// UniqueID is the discovery key: config goes to
	// homeassistant/sensor/<UniqueID>/config.
	UniqueID string
	// Name is the friendly name shown in HA.
	Name string
	// ValueTopic receives the state value (retained).
	ValueTopic  string
	Unit        string // unit_of_measurement, "" for text sensors
	StateClass  string // e.g. "temperature"; "" for text sensors
	DeviceClass string // e.g. "temperature"; "" when not applicable
	Icon        string // mdi icon
}

// Publisher manages the MQTT connection and publishes discovery configs
// plus state values for the headline sensors.
type Publisher struct {
	client       mqtt.Client
	prefix       string
	availability string // retained availability topic
	entities     []Entity
}

// DefaultEntities returns the Phase 7 headline sensor set. Topics use the
// given prefix, e.g. "jarvis/sensor/queue_voice".
func DefaultEntities(prefix string) []Entity {
	t := func(key string) string { return prefix + "/sensor/" + key }
	queue := func(key, name string) Entity {
		return Entity{
			UniqueID:   "jarvis_queue_" + key,
			Name:       name,
			ValueTopic: t("queue_" + key),
			Icon:       "mdi:tray-full",
		}
	}
	return []Entity{
		queue("voice", "Jarvis Queue: Voice"),
		queue("fix", "Jarvis Queue: Fix"),
		queue("coding", "Jarvis Queue: Coding"),
		queue("background", "Jarvis Queue: Background"),
		{
			UniqueID:   "jarvis_current_job",
			Name:       "Jarvis Current Job",
			ValueTopic: t("current_job"),
			Icon:       "mdi:console",
		},
		{
			UniqueID:   "jarvis_loaded_model",
			Name:       "Jarvis Loaded Model",
			ValueTopic: t("loaded_model"),
			Icon:       "mdi:chip",
		},
		{
			UniqueID:   "jarvis_api_spend_today",
			Name:       "Jarvis API Spend Today",
			ValueTopic: t("api_spend_today"),
			Unit:       "USD",
			Icon:       "mdi:cash-multiple",
		},
		{
			UniqueID:   "jarvis_power_cost_today",
			Name:       "Jarvis Power Cost Today",
			ValueTopic: t("power_cost_today"),
			Unit:       "USD",
			Icon:       "mdi:cash-multiple",
		},
		{
			UniqueID:    "jarvis_energy_today",
			Name:        "Jarvis Energy Today",
			ValueTopic:  t("energy_today"),
			Unit:        "kWh",
			StateClass:  "total",
			DeviceClass: "energy",
			Icon:        "mdi:battery-high",
		},
		{
			UniqueID:    "jarvis_gpu_temp",
			Name:        "Jarvis GPU Temperature",
			ValueTopic:  t("gpu_temp"),
			Unit:        "°C",
			StateClass:  "temperature",
			DeviceClass: "temperature",
			Icon:        "mdi:thermometer",
		},
		{
			UniqueID:    "jarvis_gpu_power",
			Name:        "Jarvis GPU Power",
			ValueTopic:  t("gpu_power"),
			Unit:        "W",
			StateClass:  "measurement",
			DeviceClass: "power",
			Icon:        "mdi:lightning-bolt",
		},
	}
}

// NewPublisher connects to the MQTT broker (HA_MQTT_URL, HA_MQTT_USER,
// HA_MQTT_PASS, HA_TOPIC_PREFIX env vars) and waits for the connection.
func NewPublisher() (*Publisher, error) {
	url := envOr("HA_MQTT_URL", "mqtt://localhost:1883")
	prefix := envOr("HA_TOPIC_PREFIX", "jarvis")

	opts := mqtt.NewClientOptions().
		AddBroker(url).
		SetClientID("jarvis-ha-" + pid()).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetOrderMatters(false)
	if u := os.Getenv("HA_MQTT_USER"); u != "" {
		opts.SetUsername(u)
		opts.SetPassword(os.Getenv("HA_MQTT_PASS"))
	}
	// Last will: if we die, HA marks the entities unavailable.
	opts.SetWill(prefix+"/availability", "offline", 1, true)

	client := mqtt.NewClient(opts)
	if t := client.Connect(); t.Wait() && t.Error() != nil {
		return nil, fmt.Errorf("mqtt connect %s: %w", url, t.Error())
	}
	return &Publisher{
		client:       client,
		prefix:       prefix,
		availability: prefix + "/availability",
		entities:     DefaultEntities(prefix),
	}, nil
}

// Discover publishes a retained auto-discovery config payload for every
// entity. Safe to call more than once (retained payloads overwrite).
func (p *Publisher) Discover() error {
	for _, e := range p.entities {
		cfg, err := json.Marshal(p.discoveryConfig(e))
		if err != nil {
			return err
		}
		topic := "homeassistant/sensor/" + e.UniqueID + "/config"
		if t := p.client.Publish(topic, 1, true, cfg); t.Wait() && t.Error() != nil {
			return fmt.Errorf("discovery %s: %w", e.UniqueID, t.Error())
		}
	}
	p.setOnline()
	return nil
}

func (p *Publisher) discoveryConfig(e Entity) map[string]any {
	cfg := map[string]any{
		"name":                  e.Name,
		"unique_id":             e.UniqueID,
		"state_topic":           e.ValueTopic,
		"icon":                  e.Icon,
		"value_template":        "{{ value }}",
		"availability_topic":    p.availability,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device": map[string]any{
			"identifiers":  []string{"jarvis-agent-build"},
			"name":         "Jarvis Agent",
			"manufacturer": "ImMakingAJarvis",
			"model":        "agent build",
		},
	}
	if e.Unit != "" {
		cfg["unit_of_measurement"] = e.Unit
	}
	if e.StateClass != "" {
		cfg["state_class"] = e.StateClass
	}
	if e.DeviceClass != "" {
		cfg["device_class"] = e.DeviceClass
	}
	return cfg
}

// PublishAll publishes one state value per entity from the snapshot.
// Text sensors publish "unknown" when the value is empty.
func (p *Publisher) PublishAll(s State) error {
	for _, e := range p.entities {
		val := stateValue(e, s)
		if err := p.publish(e.ValueTopic, val); err != nil {
			return err
		}
	}
	return nil
}

func stateValue(e Entity, s State) string {
	switch e.UniqueID {
	case "jarvis_queue_voice":
		return strconv0(s.Queues["voice"])
	case "jarvis_queue_fix":
		return strconv0(s.Queues["fix"])
	case "jarvis_queue_coding":
		return strconv0(s.Queues["coding"])
	case "jarvis_queue_background":
		return strconv0(s.Queues["background"])
	case "jarvis_current_job":
		if s.CurrentJob == "" {
			return "idle"
		}
		return s.CurrentJob
	case "jarvis_loaded_model":
		if s.LoadedModel == "" {
			return "unknown"
		}
		return s.LoadedModel
	case "jarvis_api_spend_today":
		return fmt.Sprintf("%.4f", s.APISpendToday)
	case "jarvis_power_cost_today":
		return fmt.Sprintf("%.4f", s.PowerCostToday)
	case "jarvis_energy_today":
		return fmt.Sprintf("%.3f", s.EnergyToday)
	case "jarvis_gpu_temp":
		if !s.GPUOK {
			return "unknown"
		}
		return fmt.Sprintf("%.0f", s.GPUTemp)
	case "jarvis_gpu_power":
		if !s.GPUOK {
			return "unknown"
		}
		return fmt.Sprintf("%.1f", s.GPUPower)
	}
	return "unknown"
}

func strconv0(n int) string { return fmt.Sprintf("%d", n) }

func (p *Publisher) publish(topic, payload string) error {
	t := p.client.Publish(topic, 1, true, payload)
	t.Wait()
	if err := t.Error(); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}
	return nil
}

// SetOnline marks all entities available (retained).
func (p *Publisher) SetOnline() { p.setOnline() }

func (p *Publisher) setOnline() {
	p.client.Publish(p.availability, 1, true, "online")
}

// SetOffline marks all entities unavailable (retained). Called on a
// deliberate stop — a clean DISCONNECT suppresses the last will, so
// without this HA would keep showing stale "online" values.
func (p *Publisher) SetOffline() {
	p.client.Publish(p.availability, 1, true, "offline")
}

// Close disconnects from the broker.
func (p *Publisher) Close() { p.client.Disconnect(250) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pid() string { return fmt.Sprintf("%d", os.Getpid()) }
