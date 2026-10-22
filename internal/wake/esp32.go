package wake

import (
	"context"
	"log"
)

// esp32Source represents a future ESP32 mic node running openWakeWord
// or microWakeWord for on-device wake word detection.
//
// This is a stub implementation. The ESP32 will stream audio or
// transcription events over a transport layer (WebSocket or UDP)
// to be filled in when the hardware is ready.
//
// Configuration (to be determined):
//   - WAKE_ESP32_ADDR: ESP32 address (default "localhost:8888")
type esp32Source struct {
	addr string
}

// NewESP32 creates an ESP32 wake-word source.
func NewESP32() Source {
	return &esp32Source{
		addr: envOr("WAKE_ESP32_ADDR", "localhost:8888"),
	}
}

func (s *esp32Source) Listen(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)

	go func() {
		<-ctx.Done()
		close(ch)
	}()

	log.Printf("wake: esp32 source initialized (addr: %s); implementation pending hardware", s.addr)

	// Stub: block until context cancellation.
	// When the ESP32 hardware is ready, replace this with the actual
	// transport (WebSocket/UDP) that receives audio or transcription
	// events from the mic node.
	<-ctx.Done()
	return ch
}
