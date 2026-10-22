// Package wake defines the abstraction for swappable wake word sources.
//
// The wake word source is responsible for detecting that the user wants to
// speak (wake word, push-to-talk, or an external trigger like Home
// Assistant's voice pipeline) and delivering the transcribed text to the
// downstream pipeline (STT → LLM → TTS).
//
// Which source is active is configured via the WAKE_SOURCE environment
// variable. Valid values:
//
//	homeassistant — Home Assistant Voice Preview Edition (default)
//	esp32         — Custom ESP32 mic node with openWakeWord / microWakeWord
//
// The source is swapped by changing config, not by changing code.
package wake

import (
	"context"
	"time"
)

// Event represents a single wake-word detection together with the
// transcribed text ready for the downstream pipeline.
type Event struct {
	// Text is the speech-to-text transcription produced by the source.
	// For Home Assistant this arrives pre-transcribed; for an ESP32 it
	// may be raw audio that the downstream pipeline transcribes.
	Text string

	// Source identifies which WakeSource produced the event
	// ("homeassistant", "esp32", ...).
	Source string

	// Timestamp is when the wake word was detected.
	Timestamp time.Time
}

// Source is the abstraction over any wake-word detection hardware or
// service. Implementations receive detection events and expose them via
// a channel so the downstream pipeline can consume them.
//
// The contract:
//   - Listen blocks until context cancellation.
//   - It returns a channel that yields Events.
//   - The channel is closed when the context is cancelled.
//   - Implementations may buffer a small number of events but should
//     not block the producer if the consumer is slow.
type Source interface {
	// Listen starts the source and returns a channel that yields
	// wake-word events. Callers should range over the returned
	// channel or select on it together with ctx.Done().
	Listen(ctx context.Context) <-chan Event
}
