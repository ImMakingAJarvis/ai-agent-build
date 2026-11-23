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
	"path/filepath"
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
	kafka := kq.New(brokers)
	m := kshadow.NewMirror(amqpURL, kafka, state, statePath, admin, sampleEvery, logf)

	// Phase 2: the shadow dry-run runs alongside the mirror (plan §5),
	// with its own state file next to the mirror's.
	dryPath := envOr("KSHADOW_DRYRUN_STATE", filepath.Join(filepath.Dir(statePath), "kshadow-dryrun.json"))
	dryState, err := kshadow.LoadDryRunState(dryPath)
	if err != nil {
		logf.Fatalf("load dry-run state: %v", err)
	}
	dry := kshadow.NewDryRun(brokers, kafka, admin, dryState, dryPath, logf)

	logf.Printf("kshadow starting (kafka=%s state=%s dry-run=%s sample-every=%d)", brokers, statePath, dryPath, sampleEvery)
	errMirror := make(chan error, 1)
	go func() { errMirror <- m.Run(ctx) }()
	errDry := make(chan error, 1)
	go func() { errDry <- dry.Run(ctx) }()
	var runErr error
	var src string
	select {
	case runErr = <-errMirror:
		src = "mirror"
	case runErr = <-errDry:
		src = "dry-run"
	}
	if runErr != nil && ctx.Err() == nil {
		logf.Fatalf("%s: %v", src, runErr)
	}
	logf.Printf("kshadow stopped")
}
