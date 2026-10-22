// wake-queue bridges wake-word detection events to the RabbitMQ job pipeline.
//
// It creates a wake-word source (configured by WAKE_SOURCE env var),
// connects to RabbitMQ, and publishes each wake event as a voice-type
// job with high priority so the worker picks it up promptly.
//
// Usage:
//
//	WAKE_SOURCE=homeassistant wake-queue
//	WAKE_SOURCE=esp32 wake-queue
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/wake"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("wake-queue: %v", err)
	}
}

func run() error {
	// Connect to RabbitMQ.
	conn, err := queue.NewConn()
	if err != nil {
		return fmt.Errorf("rabbitmq connect: %w", err)
	}
	defer conn.Close()
	log.Printf("wake-queue: connected to RabbitMQ")

	// Create the wake-word source.
	src, err := wake.NewSource()
	if err != nil {
		return fmt.Errorf("wake source: %w", err)
	}

	// Context that cancels on SIGINT/SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	events := src.Listen(ctx)
	log.Printf("wake-queue: listening for wake events")

	for ev := range events {
		log.Printf("wake-queue: wake event from %q (text: %q)", ev.Source, ev.Text)

		msg := queue.Message{
			ID:        fmt.Sprintf("wake-%d-%d", time.Now().UnixMilli(), rand.Intn(10000)),
			Type:      "voice",
			Priority:  10, // voice jobs get highest priority
			CreatedAt: ev.Timestamp,
			Source:    "wake-" + ev.Source,
			Payload: map[string]string{
				"prompt": ev.Text,
			},
		}

		routingKey := "voice.default"
		if err := conn.Publish(msg, routingKey); err != nil {
			log.Printf("wake-queue: publish failed: %v", err)
			// Try to reconnect and retry once.
			if reconnectErr := conn.Reconnect(); reconnectErr != nil {
				log.Printf("wake-queue: reconnect failed: %v", reconnectErr)
				continue
			}
			if retryErr := conn.Publish(msg, routingKey); retryErr != nil {
				log.Printf("wake-queue: retry publish failed: %v", retryErr)
				continue
			}
		}

		log.Printf("wake-queue: enqueued job %s → voice queue", msg.ID)
	}

	log.Printf("wake-queue: events channel closed, exiting")
	return nil
}
