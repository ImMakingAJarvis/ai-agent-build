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
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
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

	// Periodic queue-depth monitor: queries RabbitMQ for pending message
	// counts and feeds them to the scheduler so it can adjust which queues
	// are paused. Runs every 2 seconds.
	stopDepth := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopDepth:
				return
			case <-ticker.C:
				depths := getQueueDepths(conn)
				sched.SetDepths(depths)
			}
		}
	}()

	// Consumer handler: processes one job.
	// Evaluate queue depths once before starting, so the scheduler is in the
	// right state from the first message.
	initialDepths := getQueueDepths(conn)
	sched.SetDepths(initialDepths)

	jobStore, err := agentjob.Open(history.Dir())
	if err != nil {
		return fmt.Errorf("job store: %w", err)
	}

	handle := func(msg queue.Message) error {
		// Agent jobs are build work run by the agent-run loop. Claim means
		// write the job to the store (ready, idempotent), then ack right
		// away: never hold a message unacked for a whole round, the broker's
		// ack timeout would close the channel and redeliver it.
		if msg.Kind == queue.KindAgent {
			added, err := jobStore.Ingest(agentjob.Spec{
				ID: msg.ID, Title: msg.Title, Instructions: msg.Instructions,
				Branch: msg.Branch, DependsOn: msg.DependsOn, Driver: msg.Driver,
				Priority: msg.Priority, CreatedAt: msg.CreatedAt, Source: msg.Source,
			})
			if err != nil {
				return fmt.Errorf("ingest agent job %s: %w", msg.ID, err)
			}
			log.Printf("worker: agent job %s ingested (new=%v)", msg.ID, added)
			return nil
		}

		// Priority: a lower-priority queue waits (bounded, well under the
		// ack timeout) while a higher one has work, then goes ahead anyway.
		// A job is never skipped or acked without running.
		for waited := 0; sched.IsPaused(msg.Type) && waited < 120; waited++ {
			time.Sleep(500 * time.Millisecond)
		}

		// Check if local model is available.
		localAvailable := ollama.Healthcheck()
		log.Printf("worker: job %s routing: type=%s, forceAPI=%v, modelOverride=%q, localAvailable=%v",
			msg.ID, msg.Type, msg.ForceAPI, msg.ModelOverride, localAvailable)
		modelName, useLocal := router.PickModelWithPayload(msg.Type, msg.ForceAPI, msg.ModelOverride, localAvailable)
		log.Printf("worker: job %s routed to model=%q, useLocal=%v", msg.ID, modelName, useLocal)

		// Estimate cost for API-dispatched jobs before routing.
		prompt := msg.Payload["prompt"]
		if prompt == "" {
			prompt = "(no prompt)"
		}
		var costEst router.CostEstimate
		if !useLocal {
			costEst = router.EstimateCost(modelName, prompt, msg.Type)
			log.Printf("worker: job %s → %s estimated cost=$%.4f (in=%d tokens, out=%d tokens)",
				msg.ID, modelName, costEst.TotalCost, costEst.InputTokens, costEst.OutputTokens)

			// Approval gate: block if estimated cost exceeds threshold.
			if !router.Approved(useLocal, msg.Payload, costEst) {
				log.Printf("worker: job %s BLOCKED (estimated cost=$%.4f exceeds threshold)",
					msg.ID, costEst.TotalCost)
				// Record start briefly so we can record the denial.
				rec := history.Start(msg, modelName, useLocal, msg.Type)
				rec.EstimatedCost = costEst.TotalCost
				rec.InputTokens = costEst.InputTokens
				rec.OutputTokens = costEst.OutputTokens
				history.Denied(rec, costEst.TotalCost)
				return queue.ErrApprovalDenied
			}
		}

		// Record job start.
		rec := history.Start(msg, modelName, useLocal, msg.Type)
		if costEst.TotalCost > 0 {
			rec.EstimatedCost = costEst.TotalCost
			rec.InputTokens = costEst.InputTokens
			rec.OutputTokens = costEst.OutputTokens
		}
		sched.JobStarted(msg.Type)
		defer sched.JobFinished(msg.Type)

		if !useLocal || !localAvailable {
			// Nothing dispatches to a paid API yet: say so instead of
			// recording a fake success.
			err := fmt.Errorf("API dispatch not implemented (model %s)", modelName)
			log.Printf("worker: job %s failed: %v", msg.ID, err)
			history.Fail(rec, err)
			return nil // recorded as failed; ack so it is not redelivered
		}

		idleUnload.MarkUsed()
		log.Printf("worker: job %s → %s (prompt: %s)", msg.ID, modelName, prompt)
		resp, err := ollama.Generate(modelName, prompt)
		if err != nil {
			history.Fail(rec, err)
			return fmt.Errorf("generate: %w", err)
		}
		out := resp.Response
		if err := jobStore.WriteFile(msg.ID, "output.txt", []byte(out)); err != nil {
			log.Printf("worker: job %s: store output: %v", msg.ID, err)
		}
		rec.Summary = agentjob.Summary(out)
		history.Complete(rec)
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
	close(stopDepth)
	consumer.Stop()
	return nil
}

// getQueueDepths queries RabbitMQ for the pending message count on each
// managed queue. Returns a map of queue-name → depth.
func getQueueDepths(conn *queue.Conn) map[string]int {
	depths := make(map[string]int)
	ch := conn.Channel()
	for _, q := range queue.AllQueues {
		get, err := ch.QueueInspect(q)
		if err != nil {
			continue
		}
		depths[q] = get.Messages
	}
	return depths
}
