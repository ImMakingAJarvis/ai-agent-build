// worker is the RabbitMQ consumer that processes queued jobs.
//
// It connects to RabbitMQ, consumes from voice/coding/background queues,
// routes each job to the appropriate model via pickModel, and records
// job history (start, finish, model, duration). It also runs one
// location worker per jobs.gpu/jobs.cpu/jobs.igpu queue (A6) so jobs on
// different locations run in parallel; the game watchdog pauses only the
// GPU queue while a stream is active.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/metrics"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/notifier"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/notify"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
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

	// Engine client for local model routing (Ollama or llama.cpp).
	ollama := models.NewClient()

	// Idle unloader for the local model. A llama.cpp engine has no unload
	// API and keeps its model resident — which is what we want for the
	// dedicated brain container — so the unloader only runs for Ollama.
	var idleUnload *models.IdleUnload
	if ollama.IsOllama() {
		idleUnload = models.NewIdleUnload(ollama, "jarvis-brain")
		idleUnload.Start()
		defer idleUnload.Stop()
	} else {
		log.Printf("worker: engine is not Ollama — idle-unload disabled (llama.cpp keeps its model resident)")
	}

	// Location workers (A6): one worker per jobs.gpu/jobs.cpu/jobs.igpu
	// queue so CPU and iGPU jobs run in parallel with the GPU job instead
	// of waiting behind it. In Phase A the handler records the job and
	// acks; the model-swap / cloud dispatch that actually runs a location
	// job arrives with Phase B (B2) and C1.
	locWorkers := queue.NewLocationWorkers(conn, func(msg queue.Message) error {
		log.Printf("worker: location job %s (type=%s) received — executor lands in Phase B/C", msg.ID, msg.Type)
		return nil
	})

	// Stream watcher detects active Sunshine/Steam streams so we can free
	// GPU memory while a stream is active.
	streamCh := make(chan streamwatcher.StateChange, 1)
	streamWatcher := streamwatcher.New(streamwatcher.Config{
		SunshineEndpoint: os.Getenv("SUNSHINE_ENDPOINT"),
		SteamEndpoint:    os.Getenv("STEAM_ENDPOINT"),
	})
	streamWatcher.Start(streamCh)

	// Engine manager controls the jarvis-engine docker container.
	engMgr := engine.New("jarvis-engine")

	// streamBlocked is true while a stream is active and the engine is
	// stopped to free VRAM.  The healthcheck below reads it atomically.
	var streamBlocked atomic.Bool

	// Handle stream state transitions in the background.
	go func() {
		for change := range streamCh {
			if change.Stream != streamwatcher.NoStream {
				// Stream started — stop the engine to free ~22GB VRAM and
				// pause the GPU location queue. The game watchdog pauses
				// only the GPU queue: CPU and iGPU jobs keep running (A6).
				log.Printf("worker: stream %s started — stopping engine, pausing GPU queue", change.Stream)
				locWorkers.Pause(queue.JobsGPUQueue)
				if err := engMgr.StopService(); err != nil {
					log.Printf("worker: stop engine: %v", err)
				} else {
					streamBlocked.Store(true)
				}
			} else {
				// Stream ended — restart the engine and resume the GPU
				// location queue.
				log.Printf("worker: stream ended — restarting engine, resuming GPU queue")
				streamBlocked.Store(false)
				locWorkers.Resume(queue.JobsGPUQueue)
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

	// Periodic model info poller: queries the engine's model info every
	// 30 seconds (Ollama /api/ps or llama.cpp /v1/models) and updates the
	// model info gauges for the Grafana dashboard.
	stopModelInfo := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopModelInfo:
				return
			case <-ticker.C:
				models, err := ollama.ModelPS()
				if err != nil {
					log.Printf("worker: model info poll: %v", err)
					jobMetrics.ObserveModelInfo("", 0, 0, "", "", 0, 0, false)
					continue
				}
				if len(models) == 0 {
					jobMetrics.ObserveModelInfo("", 0, 0, "", "", 0, 0, false)
					continue
				}
				mi := models[0] // Use the first loaded model
				paramsBillions := parseParamCount(mi.Details.ParameterSize)
				jobMetrics.ObserveModelInfo(
					mi.Name, mi.SizeVRAM, mi.Size,
					mi.Details.Family, mi.Details.Quantization,
					paramsBillions, mi.ContextLength, true)
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

	// Plan worker (B2): plan_epic jobs are dispatched here to the planner
	// model named in them — a local planner is swapped in and restored, a
	// cloud planner runs on its API (internal/planworker). Without a
	// registry the worker stays up and plan jobs fall back to the
	// agent-run loop, as before B2.
	planHandler, err := newPlanHandler()
	if err != nil {
		log.Printf("worker: plan handler unavailable (%v) — plan_epic jobs fall back to the agent-run loop", err)
		planHandler = nil
	}

	// Plan delivery (B4): a finished plan goes to the board — a page with
	// the plan, the tasks as sub-items in Plan review, the epic moved
	// there. It uses the same board client as the bridge (board
	// credentials from the environment / agent.env); without them the
	// worker still plans (plan.md is written to the store) but plan jobs
	// fail on delivery.
	var planDeliverer *planworker.Deliverer
	if planHandler != nil {
		pc, derr := plane.FromEnv()
		if derr != nil {
			log.Printf("worker: plan delivery unavailable (%v) — plan_epic jobs fail on delivery", derr)
		} else {
			planDeliverer = &planworker.Deliverer{Plane: pc, Registry: planHandler.Models}
		}
	}

	// planMu serializes plan runs: a swap moves the one big model that
	// fits in VRAM, so two concurrent plan jobs must not race the engine.
	var planMu sync.Mutex

	handlePlan := func(msg queue.Message, job *planworker.Job) error {
		planMu.Lock()
		defer planMu.Unlock()

		local := true
		for i := range planHandler.Models.Models {
			if planHandler.Models.Models[i].ID == job.ModelID {
				local = planHandler.Models.Models[i].Location != "cloud"
				break
			}
		}
		rec := history.Start(msg, job.ModelID, local, msg.Type)
		log.Printf("worker: plan job %s → model %s (local=%v, effort=%s)",
			msg.ID, job.ModelID, local, job.Effort)

		jobStart := time.Now()
		res, err := planHandler.Run(context.Background(), job)
		if err != nil {
			log.Printf("worker: plan job %s failed: %v", msg.ID, err)
			history.Fail(rec, err)
			ntf.OnFail(rec, err)
			jobMetrics.ObserveFailed(msg.Type, job.ModelID)
			resolvePlanJobStore(jobStore, msg, job.ModelID, "", err.Error())
			return nil // recorded as failed; ack so it is not redelivered
		}

		if err := jobStore.WriteFile(msg.ID, "plan.md", []byte(res.Plan)); err != nil {
			log.Printf("worker: plan job %s: store plan: %v", msg.ID, err)
		}

		// Deliver the plan to the board (B4). The pipeline already wrote
		// plan.md, so a delivery failure records the job as failed — the
		// plan exists in the store, the board is not.
		if planDeliverer == nil {
			derr := fmt.Errorf("plan delivery unavailable (no board credentials)")
			log.Printf("worker: plan job %s: %v", msg.ID, derr)
			history.Fail(rec, derr)
			ntf.OnFail(rec, derr)
			jobMetrics.ObserveFailed(msg.Type, job.ModelID)
			resolvePlanJobStore(jobStore, msg, job.ModelID, "", derr.Error())
			return nil // recorded as failed; ack so it is not redelivered
		}
		delivery, derr := planDeliverer.Deliver(context.Background(), job, res)
		if derr != nil {
			log.Printf("worker: plan job %s: delivery failed: %v", msg.ID, derr)
			history.Fail(rec, derr)
			ntf.OnFail(rec, derr)
			jobMetrics.ObserveFailed(msg.Type, job.ModelID)
			resolvePlanJobStore(jobStore, msg, job.ModelID, "", derr.Error())
			return nil // recorded as failed; ack so it is not redelivered
		}
		log.Printf("worker: plan job %s: plan delivered (page %s, %d sub-item(s))",
			msg.ID, delivery.Page.ID, len(delivery.Items))

		rec.Summary = agentjob.Summary(res.Plan)
		history.Complete(rec)
		jobMetrics.ObserveCompleted(msg.Type, job.ModelID, time.Since(jobStart).Seconds())
		ntf.OnComplete(rec)
		resolvePlanJobStore(jobStore, msg, job.ModelID, res.Plan, "")
		return nil
	}

	handle := func(msg queue.Message) error {
		// Agent jobs are build work run by the agent-run loop. Claim means
		// write the job to the store (ready, idempotent), then ack right
		// away: never hold a message unacked for a whole round, the broker's
		// ack timeout would close the channel and redeliver it.
		if msg.Kind == queue.KindAgent {
			// A plan_epic job is run here, not by the agent-run loop (B2):
			// it dispatches to the planner model named in the job.
			if job, perr := planworker.Parse(msg.Instructions); perr == nil {
				if planHandler == nil {
					log.Printf("worker: plan job %s: no plan handler — leaving it to the agent-run loop", msg.ID)
				} else {
					return handlePlan(msg, job)
				}
			}
			spec := agentjob.Spec{
				ID: msg.ID, Title: msg.Title, Instructions: msg.Instructions,
				Branch: msg.Branch, DependsOn: msg.DependsOn, Driver: msg.Driver,
				Class: msg.Class, Priority: msg.Priority, CreatedAt: msg.CreatedAt, Source: msg.Source,
				ReasoningEffort: msg.ReasoningEffort,
			}
			if msg.Type == queue.ImageQueue {
				photoSpecDefaults(&spec, msg.Payload["prompt"])
			}
			added, err := jobStore.Ingest(spec)
			if err != nil {
				return fmt.Errorf("ingest agent job %s: %w", msg.ID, err)
			}
			log.Printf("worker: agent job %s ingested (new=%v)", msg.ID, added)
			return nil
		}

		// Image jobs (photo generation) never run as text jobs — the photo
		// pipeline (docs/art-model.md, docs/brain-swap.md) runs them as agent
		// rounds. A plain {type: image, payload: {prompt}} message becomes an
		// agent job too, so publishing to the image queue always lands in the
		// photo pipeline instead of a prompt for the local model.
		if msg.Type == queue.ImageQueue {
			spec := agentjob.Spec{
				ID: msg.ID, Title: msg.Title, Instructions: msg.Instructions,
				Branch: msg.Branch, DependsOn: msg.DependsOn, Driver: msg.Driver,
				Class: msg.Class, Priority: msg.Priority, CreatedAt: msg.CreatedAt, Source: msg.Source,
				ReasoningEffort: msg.ReasoningEffort,
			}
			photoSpecDefaults(&spec, msg.Payload["prompt"])
			added, err := jobStore.Ingest(spec)
			if err != nil {
				return fmt.Errorf("ingest image job %s: %w", msg.ID, err)
			}
			log.Printf("worker: image job %s ingested as agent job (new=%v, class=%s, priority=%d, driver=%s)",
				msg.ID, added, spec.Class, spec.Priority, spec.Driver)
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

		if idleUnload != nil {
			idleUnload.MarkUsed()
		}
		log.Printf("worker: job %s → %s (prompt: %s)", msg.ID, modelName, prompt)
		resp, err := ollama.Generate(modelName, prompt)
		if err != nil {
			history.Fail(rec, err)
			jobMetrics.ObserveFailed(msg.Type, modelName)
			return fmt.Errorf("generate: %w", err)
		}
		// Record model run metrics: request count, token counts, and eval times.
		jobMetrics.ObserveRequest()
		jobMetrics.ObserveTokens(resp.PromptEvalCount, resp.EvalCount,
			time.Duration(resp.PromptEvalDuration), time.Duration(resp.EvalDuration))
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

	// Start location workers (one per jobs.gpu/jobs.cpu/jobs.igpu queue).
	if err := locWorkers.Start(); err != nil {
		return fmt.Errorf("location workers start: %w", err)
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
	close(stopModelInfo)
	streamWatcher.Stop()
	close(streamCh)
	consumer.Stop()
	locWorkers.Stop()
	return nil
}

// newPlanHandler builds the B2 plan dispatcher: the registry from
// WORKER_MODELS_FILE (default config/models.yaml, repo root), one engine
// client per registry endpoint, the cloud caller for the registry's cloud
// engines, and the B3 planning pipeline (the Plan/Decompose/Review prompts
// plus the task JSON schema, prompts/).
//
// When the worker runs beside the engine (docker compose), the registry's
// host-local endpoints (localhost:11434) are not reachable from the
// container, so ENGINE_URL / OLLAMA_URL take precedence over them — the
// same convention as models.NewClient.
func newPlanHandler() (*planworker.Handler, error) {
	regPath := os.Getenv("WORKER_MODELS_FILE")
	if regPath == "" {
		regPath = "config/models.yaml"
	}
	reg, err := registry.Load(regPath)
	if err != nil {
		return nil, fmt.Errorf("load model registry %s: %w", regPath, err)
	}
	engineBase := os.Getenv("ENGINE_URL")
	if engineBase == "" {
		engineBase = os.Getenv("OLLAMA_URL")
	}
	engines := map[string]planworker.Engine{}
	var mu sync.Mutex
	frontEnd := os.Getenv("JARVIS_FRONTEND_MODEL")
	if frontEnd == "" {
		// The iGPU helper, by the name its llama.cpp engine reports.
		frontEnd = "jarvis-igpu.gguf"
	}
	promptsDir := os.Getenv("WORKER_PROMPTS_DIR")
	if promptsDir == "" {
		promptsDir = "prompts"
	}
	// WORKER_REPO_ROOT is the repo the plan step summarises for the model
	// (the worker runs with the repo root as its CWD by default).
	pipe, err := planworker.NewPipeline(promptsDir, os.Getenv("WORKER_REPO_ROOT"), reg)
	if err != nil {
		return nil, fmt.Errorf("planning pipeline: %w", err)
	}
	return &planworker.Handler{
		Models: reg,
		Engines: func(endpoint string) planworker.Engine {
			base := endpoint
			if engineBase != "" {
				base = engineBase
			}
			mu.Lock()
			defer mu.Unlock()
			if e, ok := engines[base]; ok {
				return e
			}
			e := planworker.NewClientEngine(base)
			engines[base] = e
			return e
		},
		Cloud:    planworker.NewAPI(),
		Planner:  pipe,
		FrontEnd: frontEnd,
	}, nil
}

// resolvePlanJobStore settles a plan job's agentjob store entry so the
// agent-run loop never picks it up as a build round: B1 ingests plan jobs
// as ready (store-only anchor) and the loop claims ready jobs. A missing
// entry (the bridge ingest has not landed) is created first, so the store
// stays whole.
func resolvePlanJobStore(store *agentjob.Store, msg queue.Message, model, summary, runErr string) {
	st := agentjob.State{
		Status:     agentjob.Done,
		Model:      model,
		Summary:    summary,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if runErr != "" {
		st.Status = agentjob.Failed
		st.Error = runErr
		st.Summary = ""
	}
	if _, err := store.Get(msg.ID); err != nil {
		if _, ierr := store.Ingest(agentjob.Spec{
			ID: msg.ID, Title: msg.Title, Instructions: msg.Instructions,
			Branch: msg.Branch, DependsOn: msg.DependsOn, Driver: msg.Driver,
			Class: msg.Class, Priority: msg.Priority, CreatedAt: msg.CreatedAt, Source: msg.Source,
			ReasoningEffort: msg.ReasoningEffort,
		}); ierr != nil {
			log.Printf("worker: plan job %s: store ingest for resolution: %v", msg.ID, ierr)
		}
	}
	if err := store.SetStatus(msg.ID, st); err != nil {
		log.Printf("worker: plan job %s: store resolution: %v", msg.ID, err)
	}
}

// photoSpecDefaults fills the photo-pipeline defaults into an image job's
// spec when the message left a field empty: the prompt becomes the
// title/instructions, the job runs as class fix at the front of the band
// (photo jobs almost always take priority), and on the igpu driver — the
// brain-swap window is what runs photo jobs (docs/brain-swap.md).
func photoSpecDefaults(spec *agentjob.Spec, prompt string) {
	if spec.Title == "" {
		spec.Title = prompt
	}
	if spec.Instructions == "" {
		spec.Instructions = prompt
	}
	if spec.Class == "" {
		spec.Class = agentjob.ClassFix
	}
	if spec.Priority == 0 {
		spec.Priority = 100
	}
	if spec.Driver == "" {
		spec.Driver = "igpu"
	}
	// The "photo" tag (agentjob.TagPhoto once it lands on main) is what
	// the queue watcher keys on to bracket the batch with a brain-swap
	// window (scripts/agent-run) — photo work never runs with the brain
	// on the 4090.
	if !spec.HasTag("photo") {
		spec.Tags = append(spec.Tags, "photo")
	}
}

// getQueueDepths queries RabbitMQ for the pending message count on each
// managed queue. Returns a map of queue-name → depth.
func getQueueDepths(conn *queue.Conn) map[string]int {
	depths := make(map[string]int)
	for _, q := range queue.AllQueues {
		n, err := conn.QueueDepth(q)
		if err != nil {
			continue
		}
		depths[q] = n
	}
	return depths
}

// parseParamCount converts a parameter size string like "27.3B" or "7B"
// into a float64 representing billions of parameters.
func parseParamCount(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	suffix := string(s[len(s)-1])
	var multiplier float64
	switch suffix {
	case "B", "b":
		multiplier = 1.0
		s = s[:len(s)-1]
	case "M", "m":
		multiplier = 0.001
		s = s[:len(s)-1]
	default:
		// No recognized suffix — treat as billions directly.
		multiplier = 1.0
	}
	var n float64
	_, err := fmt.Sscanf(s, "%f", &n)
	if err != nil {
		return 0
	}
	return n * multiplier
}
