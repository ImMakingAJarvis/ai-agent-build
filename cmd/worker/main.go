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
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/metrics"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/notifier"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/notify"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/router"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/streamwatcher"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	// Stream watcher detects active Sunshine/Steam streams so we can free
	// GPU memory while a stream is active.
	streamCh := make(chan streamwatcher.StateChange, 1)
	streamWatcher := streamwatcher.New(streamwatcher.Config{
		SunshineEndpoint: os.Getenv("SUNSHINE_ENDPOINT"),
		SteamEndpoint:    os.Getenv("STEAM_ENDPOINT"),
	})
	streamWatcher.Start(streamCh)

	// Engine manager controls the jarvis-engine systemd service.
	engMgr := engine.New("jarvis-engine.service")

	// streamBlocked is true while a stream is active and the engine is
	// stopped to free VRAM.  The healthcheck below reads it atomically.
	var streamBlocked atomic.Bool

	// Handle stream state transitions in the background.
	go func() {
		for change := range streamCh {
			if change.Stream != streamwatcher.NoStream {
				// Stream started — stop the engine to free ~22GB VRAM.
				log.Printf("worker: stream %s started — stopping engine", change.Stream)
				if err := engMgr.StopService(); err != nil {
					log.Printf("worker: stop engine: %v", err)
				} else {
					streamBlocked.Store(true)
				}
			} else {
				// Stream ended — restart the engine.
				log.Printf("worker: stream ended — restarting engine")
				streamBlocked.Store(false)
				if err := engMgr.StartService(); err != nil {
					log.Printf("worker: start engine: %v", err)
				}
			}
		}
	}()

	// Job history recorder.
	history, err := queue.NewHistory()
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	defer history.Close()
	log.Printf("worker: job history → %s", history.Path())

	// Prometheus metrics collector.
	jobMetrics := metrics.New()

	// Home Assistant notification client.
	haClient := notify.NewClient()
	ntf := notifier.New(haClient)
	if haClient.Configured() {
		log.Printf("worker: Home Assistant notifications enabled")
	} else {
		log.Printf("worker: Home Assistant notifications disabled (no HA_TOKEN)")
	}

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
				ReasoningEffort: msg.ReasoningEffort,
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

		jobStart := time.Now()

		// Check if local model is available.  streamBlocked is true while
		// a Sunshine/Steam stream is active and we've stopped the engine
		// to free VRAM.
		localAvailable := ollama.Healthcheck() && !streamBlocked.Load()
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
				ntf.OnDenied(rec, costEst.TotalCost)
				jobMetrics.ObserveDenied(msg.Type, modelName, costEst.TotalCost)
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
			ntf.OnFail(rec, err)
			jobMetrics.ObserveFailed(msg.Type, modelName)
			return nil // recorded as failed; ack so it is not redelivered
		}

		idleUnload.MarkUsed()
		log.Printf("worker: job %s → %s (prompt: %s)", msg.ID, modelName, prompt)
		resp, err := ollama.Generate(modelName, prompt)
		if err != nil {
			history.Fail(rec, err)
			jobMetrics.ObserveFailed(msg.Type, modelName)
			return fmt.Errorf("generate: %w", err)
		}
		out := resp.Response
		if err := jobStore.WriteFile(msg.ID, "output.txt", []byte(out)); err != nil {
			log.Printf("worker: job %s: store output: %v", msg.ID, err)
		}
		rec.Summary = agentjob.Summary(out)
		history.Complete(rec)
		jobMetrics.ObserveCompleted(msg.Type, modelName, time.Since(jobStart).Seconds())
		ntf.OnComplete(rec)
		return nil
	}

	// Start Prometheus metrics HTTP server.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: ":9101", Handler: metricsMux}
	go func() {
		log.Printf("worker: metrics listening on :9101")
		if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("worker: metrics server error: %v", err)
		}
	}()

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
	streamWatcher.Stop()
	close(streamCh)
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
