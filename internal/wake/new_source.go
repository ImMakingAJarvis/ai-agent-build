package wake

import (
	"fmt"
	"os"
)

// NewSource creates a wake-word source based on the WAKE_SOURCE
// environment variable. If unset, it defaults to "homeassistant".
//
// Recognized values:
//   - "homeassistant" (default): Home Assistant Voice Preview Edition
//   - "esp32": ESP32 mic node with openWakeWord / microWakeWord
func NewSource() (Source, error) {
	name := os.Getenv("WAKE_SOURCE")
	if name == "" {
		name = "homeassistant"
	}

	switch name {
	case "homeassistant":
		return NewHomeAssistant(), nil
	case "esp32":
		return NewESP32(), nil
	default:
		return nil, fmt.Errorf("wake: unknown source %q", name)
	}
}
