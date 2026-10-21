// worker is the RabbitMQ consumer that processes queued jobs.
//
// It connects to RabbitMQ, consumes from voice/coding/background queues,
// routes each job to the appropriate model via pickModel, and records
// job history (start, finish, model, duration).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/router"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func run() error {
	flag.Parse()

	// Connect to RabbitMQ.
	conn, err := queue.NewConn()
	if err != nil {
		return fmt.Errorf("rabbitmq connect: %w", err)
	}
	defer conn.Close()
	log.Printf("worker: connected to RabbitMQ")

	// Ollama client for local model routing.
	ollama := models.NewClient()

	// Idle unloader for the local model.
	idleUnload := models.NewIdleUnload(ollama, "jarvis-brain")
	idleUnload.Start()
	defer idleUnload.Stop()

	// Job history recorder.
	history, err := queue.NewHistory()
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	defer history.Close()
	log.Printf("worker: job history → %s", history.Path())

	// Priority scheduler.
	sched := queue.NewScheduler()

	// Consumer handler: processes one job.
	handle := func(msg queue.Message) error {
		// Check if this queue is paused by the scheduler.
		if sched.IsPaused(msg.Type) {
			log.Printf("worker: queue %s is paused by scheduler, skipping", msg.Type)
			return nil
		}

		// Check if local model is available.
		localAvailable := ollama.Healthcheck()
		modelName, useLocal := router.PickModelWithPayload(msg.Type, msg.Payload, localAvailable)

		// Record job start.
		rec := history.Start(msg, modelName, useLocal, msg.Type)
		sched.JobStarted(msg.Type)

		// If local, mark as used (resets idle timer) and generate.
		if useLocal && localAvailable {
			idleUnload.MarkUsed()
			prompt := msg.Payload["prompt"]
			if prompt == "" {
				prompt = "(no prompt)"
			}
			log.Printf("worker: job %s → %s (prompt: %s)", msg.ID, modelName, prompt)
			_, err := ollama.Generate(modelName, prompt)
			if err != nil {
				history.Fail(rec, err)
				return fmt.Errorf("generate: %w", err)
			}
		} else {
			log.Printf("worker: job %s → %s (API dispatch; not yet implemented)", msg.ID, modelName)
		}

		// Record job completion.
		history.Complete(rec)
		sched.JobFinished(msg.Type)
		return nil
	}

	// Start consumer.
	consumer := queue.NewConsumer(conn, handle)
	if err := consumer.Start(); err != nil {
		return fmt.Errorf("consumer start: %w", err)
	}

	log.Printf("worker: started — listening for jobs")

	// Graceful shutdown on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("worker: shutting down")
	consumer.Stop()
	return nil
}
