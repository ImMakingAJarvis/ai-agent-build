// task-runner is the C1 execution loop host service (Phase C of
// docs/work-board/PLAN.md): it consumes the location queues
// (jobs.gpu/jobs.cpu/jobs.igpu) that plane-bridge publishes run_task jobs
// onto and drives each card to Done or Blocked — branch, worktree, Aider,
// acceptance check, commit, merge, NOTES.md line, board comments.
//
// It runs on the host, not in the worker container: the worker mounts the
// repo read-only and carries no git, no Python, no Go toolchain, while the
// loop needs a writable worktree, the aider venv, and the Go toolchain for
// the build/lint gate. The worker's placeholder location consumers (which
// would have been competing consumers for these queues) were removed with
// C1.
//
// Environment (from ~/.config/jarvis/agent.env):
//
//	RABBITMQ_URL          AMQP URL (optional — the queue package's default
//	                      is the host port 127.0.0.1:5672, like the bridge)
//	PLANE_URL / PLANE_API_KEY / PLANE_WORKSPACE / PLANE_BOT_EMAIL /
//	PLANE_BOT_PASSWORD    board access (cards, states, comments — the bot)
//	TASK_RUNNER_REPO      repo root the worktrees live under
//	                      (default /home/agent/ai-agent-build)
//	TASK_RUNNER_MODELS_FILE  model registry (default <repo>/config/models.yaml)
//	JARVIS_DATA_DIR       agent job store dir (default ~/.local/share/jarvis —
//	                      the same store plane-bridge writes into)
//	SUNSHINE_ENDPOINT / STEAM_ENDPOINT  optional extras for the game
//	                      watchdog (it always runs: Sunshine/Steam are
//	                      detected via /proc, NVENC utilization as a
//	                      fallback) — while a stream is active the GPU
//	                      location queue is paused, so GPU jobs wait in
//	                      the lane
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/streamwatcher"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/taskrunner"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "task-runner")
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	repo := envOr("TASK_RUNNER_REPO", "/home/agent/ai-agent-build")
	modelsFile := envOr("TASK_RUNNER_MODELS_FILE", filepath.Join(repo, "config", "models.yaml"))

	client, err := plane.FromEnv()
	if err != nil {
		return fmt.Errorf("plane client: %w", err)
	}
	reg, err := registry.Load(modelsFile)
	if err != nil {
		return fmt.Errorf("model registry: %w", err)
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return fmt.Errorf("opening job store: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The broker may be down at boot (docker restarts after a crash);
	// retry rather than flap through systemd.
	var conn *queue.Conn
	for {
		conn, err = queue.NewConn()
		if err == nil {
			break
		}
		log.Warn("rabbitmq unavailable, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	defer conn.Close()
	log.Info("connected to rabbitmq")

	// Placement at run time: live memory readings plus the box locations
	// detected at boot (the runner re-decides, so a bridge placement made
	// under different VRAM pressure is not binding).
	present := placement.Detect(ctx)
	log.Info("box locations detected", "locations", placement.Names(present))
	live := func(ctx context.Context) (placement.State, error) {
		r, err := fit.Live{}.Readings(ctx)
		if err != nil {
			return placement.State{}, err
		}
		return placement.State{FreeVRAMGB: r.FreeVRAMGB, FreeRAMGB: r.FreeRAMGB, Present: present}, nil
	}

	runner := &taskrunner.Runner{
		Board:       client,
		Registry:    reg,
		Repo:        repo,
		Harness:     taskrunner.Aider{Log: log},
		Checks:      taskrunner.ShellChecker{},
		Live:        live,
		MaxAttempts: 3,
		Log:         log,
	}

	// Settle the job's store entry from the run result. A missing entry
	// (the bridge ingest has not landed) is created first, so the store
	// stays whole — the same pattern as the worker's plan-job resolution.
	settle := func(msg queue.Message, st agentjob.State) {
		if _, err := store.Get(msg.ID); err != nil {
			if _, ierr := store.Ingest(agentjob.Spec{
				ID: msg.ID, Title: msg.Title, Instructions: msg.Instructions,
				Class: msg.Class, Priority: msg.Priority, CreatedAt: msg.CreatedAt, Source: msg.Source,
			}); ierr != nil {
				log.Error("store ingest for resolution", "job", msg.ID, "err", ierr)
			}
		}
		if err := store.SetStatus(msg.ID, st); err != nil {
			log.Error("store resolution", "job", msg.ID, "err", err)
		}
	}

	handler := func(msg queue.Message) error {
		job, err := taskrunner.Parse([]byte(msg.Instructions))
		if err != nil {
			// Garbage on the location queue: settle the entry (if any)
			// failed and ack — a nack without requeue would just drop it,
			// and requeueing would loop forever.
			log.Error("run_task message rejected", "job", msg.ID, "err", err)
			settle(msg, agentjob.State{Status: agentjob.Failed, Error: err.Error(), FinishedAt: now()})
			return nil
		}
		log.Info("run_task picked up", "job", msg.ID, "item", job.Item.ID,
			"name", job.Item.Name, "model", job.ModelID())
		settle(msg, agentjob.State{Status: agentjob.Running, PID: os.Getpid(), StartedAt: now()})

		res, err := runner.Run(ctx, job)
		switch {
		case err != nil:
			// Infrastructure failure (board unreachable, git error): the
			// card state is unknown, so the entry fails and the bridge's
			// catch-up re-triggers it (terminal entry + card state).
			log.Error("run_task failed", "job", msg.ID, "err", err)
			settle(msg, agentjob.State{Status: agentjob.Failed, Error: err.Error(), FinishedAt: now()})
		case res.Waiting:
			// Deps not done (or cloud, not wired yet): the card stays
			// Ready; skipped is the terminal "settled without a verdict"
			// state, so the bridge's next catch-up re-triggers the job.
			log.Info("run_task waiting", "job", msg.ID, "why", res.Summary)
			settle(msg, agentjob.State{Status: agentjob.Skipped, Summary: res.Summary, FinishedAt: now()})
		case res.Blocked:
			log.Info("run_task blocked", "job", msg.ID, "why", res.Summary)
			settle(msg, agentjob.State{
				Status: agentjob.Done, Summary: "blocked: " + res.Summary,
				Error: res.Error, FinishedAt: now(),
			})
		case res.Passed:
			log.Info("run_task done", "job", msg.ID, "summary", res.Summary)
			settle(msg, agentjob.State{Status: agentjob.Done, Summary: res.Summary, FinishedAt: now()})
		default:
			log.Error("run_task ended without a result", "job", msg.ID)
			settle(msg, agentjob.State{Status: agentjob.Failed,
				Error: "run ended without a result", FinishedAt: now()})
		}
		return nil
	}

	lw := queue.NewLocationWorkers(conn, handler)
	if err := lw.Start(); err != nil {
		return fmt.Errorf("location workers: %w", err)
	}
	defer lw.Stop()

	// Game watchdog for the queue lane (the worker container stops the
	// engine to free VRAM; the task-runner pauses the GPU location queue,
	// so GPU jobs wait in the lane while a game holds the GPU). The
	// watcher always runs — Sunshine/Steam are detected via /proc with
	// NVENC utilization as a fallback; the endpoints are optional extras.
	streamCh := make(chan streamwatcher.StateChange, 1)
	streamWatcher := streamwatcher.New(streamwatcher.Config{
		SunshineEndpoint: os.Getenv("SUNSHINE_ENDPOINT"),
		SteamEndpoint:    os.Getenv("STEAM_ENDPOINT"),
	})
	streamWatcher.Start(streamCh)
	go func() {
		for change := range streamCh {
			if change.Stream != streamwatcher.NoStream {
				log.Info("stream started — pausing the GPU location queue", "stream", change.Stream)
				lw.Pause(queue.JobsGPUQueue)
			} else {
				log.Info("stream ended — resuming the GPU location queue")
				lw.Resume(queue.JobsGPUQueue)
			}
		}
	}()

	log.Info("task-runner listening", "queues", queue.LocationQueues, "repo", repo)
	<-ctx.Done()
	log.Info("task-runner stopping")
	return nil
}
