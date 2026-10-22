package wake

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// homeAssistantSource receives transcribed text from Home Assistant's
// voice pipeline via an HTTP POST endpoint. Home Assistant Voice Preview
// Edition handles wake word detection and speech-to-text, then forwards
// the transcription to this endpoint.
//
// Configuration:
//   - WAKE_HTTP_ADDR: bind address (default ":8081")
//   - WAKE_HTTP_PATH : endpoint path (default "/wake")
type homeAssistantSource struct {
	addr string
	path string
}

// HomeAssistantRequest is the JSON body Home Assistant sends when it
// detects a wake word and transcribes speech.
type HomeAssistantRequest struct {
	Text string `json:"text"`
}

// NewHomeAssistant creates a Home Assistant wake-word source.
func NewHomeAssistant() Source {
	return &homeAssistantSource{
		addr: envOr("WAKE_HTTP_ADDR", ":8081"),
		path: envOr("WAKE_HTTP_PATH", "/wake"),
	}
}

func (s *homeAssistantSource) Listen(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)

	mux := http.NewServeMux()
	mux.HandleFunc(s.path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		var req HomeAssistantRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		ev := Event{
			Text:      req.Text,
			Source:    "homeassistant",
			Timestamp: time.Now(),
		}
		select {
		case ch <- ev:
			w.WriteHeader(http.StatusOK)
		default:
			// Channel full — log and drop to avoid blocking the HTTP handler.
			log.Printf("wake: homeassistant event dropped (channel full)")
			w.WriteHeader(http.StatusOK)
		}
	})

	server := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		if err := server.Shutdown(context.Background()); err != nil {
			log.Printf("wake: homeassistant shutdown: %v", err)
		}
		close(ch)
	}()

	log.Printf("wake: homeassistant listening on %s%s", s.addr, s.path)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("wake: homeassistant: %v", err)
		close(ch)
	}

	return ch
}

// envOr returns the environment variable value, or fallback if unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
