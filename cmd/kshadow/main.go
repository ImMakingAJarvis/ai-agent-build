// kshadow is the phase-1 dual-write mirror (docs/kafka-migration-plan.md
// §5): it taps the tasks and events exchanges through the kq.mirror AMQP
// queue and mirrors every message to Kafka with header source=amqp,
// recording per-queue watermarks on kq.watermark. It never touches the
// live consumer path — it is a subscriber of the exchanges only.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kshadow"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	logf := log.New(os.Stderr, "kshadow: ", log.LstdFlags|log.LUTC)
	amqpURL := envOr("RABBITMQ_URL", "amqp://jarvis:jarvis-local@localhost:5672")
	brokers := envOr("KAFKA_BROKERS", "127.0.0.1:9092")
	statePath := envOr("KSHADOW_STATE", "/data/kshadow-state.json")
	sampleEvery := 25 // read-back one of every 25th write (plus the first per queue)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	state, err := kshadow.LoadState(statePath)
	if err != nil {
		logf.Fatalf("load state: %v", err)
	}
	admin, err := kshadow.NewAdmin(envOr("KSHADOW_RABBITMQ_MGMT", amqpURL))
	if err != nil {
		logf.Fatalf("management client: %v", err)
	}
	m := kshadow.NewMirror(amqpURL, kq.New(brokers), state, statePath, admin, sampleEvery, logf)
	logf.Printf("kshadow starting (kafka=%s state=%s sample-every=%d)", brokers, statePath, sampleEvery)
	if err := m.Run(ctx); err != nil && ctx.Err() == nil {
		logf.Fatalf("mirror: %v", err)
	}
	logf.Printf("kshadow stopped")
}
