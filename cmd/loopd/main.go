// loopd is the event-loop agent runtime daemon. The ELR-6 skeleton
// served only /healthz and /metrics; card ELR-98 (M4.1f) makes it the
// daemon: it reads the bearer token (failing to start when the file is
// unusable), serves the runtime API, runs the scheduler loop, and on
// SIGTERM stops taking new work, waits for the running tick and its ops
// within the grace period, shuts the HTTP server down, and exits 0.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/api"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/llamacpp"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/lock"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/tools"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/workspace"
)

// version is what the API reports the daemon was built with.
const version = "dev"

// pollFloor is the shortest no-work sleep of the scheduler. The loop's
// IdleWhileReady reports the idle it has measured so far, which is zero
// on a fresh daemon — sleeping zero would busy-spin the scheduler.
const pollFloor = 200 * time.Millisecond

// loopDriver is the part of the runtime the daemon drives: one tick at a
// time, the idle it has measured, the submit path the API hands new task
// ids to, and the op-done feed. *loop.Loop implements it; a test passes a
// fake.
type loopDriver interface {
	Step(ctx context.Context) (bool, error)
	IdleWhileReady() time.Duration
	Submit(jobID string) error
	HandleOpDone(jobID, opID string, res ops.Result) error
}

// deps is what run drives: the pieces of the runtime, built by newDeps in
// main and by a test alike.
type deps struct {
	store    *agentjob.Store
	runner   *ops.Runner
	broker   *api.Broker
	drainer  *api.Drainer
	redactor *redact.Redactor
	loop     loopDriver
	metrics  *prometheus.Registry
}

func main() {
	cfg, err := config.Load(configPath())
	if err != nil {
		log.Fatalf("loopd: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("loopd: %v", err)
	}
	// One runtime at a time (card ELR-87): the lock lives in the job
	// store directory, the one directory loopd in its container and
	// loopctl on the host both see. The kernel releases the flock when
	// this process dies, so a crash never leaves a stale lock.
	lockPath := filepath.Join(agentjob.DataDir(), "loop.lock")
	lk, err := lock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			fmt.Fprintf(os.Stderr, "loopd: another runtime holds %s: %s\n", lockPath, lock.Holder(lockPath))
			os.Exit(3)
		}
		log.Fatalf("loopd: lock %s: %v", lockPath, err)
	}
	defer lk.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := newDeps(cfg)
	if err != nil {
		log.Fatalf("loopd: %v", err)
	}
	if err := run(ctx, cfg, d); err != nil {
		log.Fatalf("loopd: %v", err)
	}
}

// configPath is the runtime config file: $LOOPD_CONFIG, defaulting to
// ~/.config/jarvis/loopd.yaml (M2.4, ELR-23) — the same file loopctl
// reads, so the daemon and the one-shot runner share one config.
func configPath() string {
	if p := os.Getenv("LOOPD_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/jarvis/loopd.yaml"
	}
	return filepath.Join(home, ".config", "jarvis", "loopd.yaml")
}

// newDeps builds the runtime pieces run drives: the store, the engine,
// the slot manager, the tool registry, the op runner, the broker and the
// drainer, the redactor, and the loop with its private metrics.
func newDeps(cfg config.Config) (deps, error) {
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return deps{}, fmt.Errorf("open store: %w", err)
	}

	eng := llamacpp.New(cfg.Engine.URL)
	mgr := slot.New(
		slot.Config{Slots: cfg.Slots, SnapshotMinTokens: cfg.SnapshotMinTokens},
		eng,
		storeJobs{store: store},
	)

	// The daemon has no single workspace of its own: jobs carry theirs,
	// and the guard roots the file tools in the job store, the one
	// directory the daemon owns.
	guard := &perm.Guard{WorkDir: agentjob.DataDir()}
	toolList := make([]ops.Tool, 0, 8)
	toolList = append(toolList, tools.FileTools(guard)...)
	toolList = append(toolList, tools.ShellTool(guard))
	toolList = append(toolList, tools.FetchTool())
	// The spill store is the ops directory under each job dir: a spilled
	// handle is the path of the full text, so reading it back is a
	// ReadFile (the same wiring loopctl uses).
	toolList = append(toolList, tools.SearchOutput{Open: func(handle string) (string, error) {
		b, err := os.ReadFile(handle)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}})
	reg := ops.NewRegistry(toolList...)

	// Tool output is scrubbed before it is stored or shown: the known
	// values come from the environment and the secrets store (card
	// ELR-91).
	redactValues := redact.FromEnv(os.Environ())
	if home, err := os.UserHomeDir(); err == nil {
		redactValues = append(redactValues, redact.FromDir(filepath.Join(home, ".config", "jarvis", "secrets"))...)
	}
	red := redact.New(redactValues)

	// The loop records into its private registry, served at /metrics —
	// the same registry the skeleton already served (card ELR-33).
	m, metrics := loop.NewMetrics()
	runner := &ops.Runner{
		Registry: reg,
		Timeouts: map[ops.Class]time.Duration{
			ops.Quick:  time.Duration(cfg.Timeouts.QuickSeconds) * time.Second,
			ops.Normal: time.Duration(cfg.Timeouts.NormalSeconds) * time.Second,
		},
	}
	l := loop.New(loop.Deps{
		Engine:   eng,
		Slots:    mgr,
		Runner:   runner,
		Registry: reg,
		Store:    store,
		Config:   cfg,
		Now:      time.Now,
		Metrics:  m,
		Redact:   red.Apply,
	})

	return deps{
		store:    store,
		runner:   runner,
		broker:   api.NewBroker(64),
		drainer:  api.NewDrainer(),
		redactor: red,
		loop:     l,
		metrics:  metrics,
	}, nil
}

// run drives the daemon until ctx ends: it loads the token (failing to
// start when the file is unusable), serves the API on loopd.listen (the
// old LOOPD_ADDR env still overrides it), and runs the scheduler. On the
// way out it starts the drain, waits up to cfg.Loopd.ShutdownGraceSeconds
// for the running tick and its ops (the ones left are named in the log
// line), shuts the HTTP server down, and returns nil — main releases the
// lock and the process exits 0.
func run(ctx context.Context, cfg config.Config, d deps) error {
	token, err := api.LoadToken(cfg.Loopd.TokenFile)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	listen := cfg.Loopd.Listen
	if env := os.Getenv("LOOPD_ADDR"); env != "" {
		listen = env // the old env override (ELR-6) still wins.
	}

	// The runner's callbacks feed the event stream (card ELR-96): every
	// progress and done is published, and a done op feeds its result
	// back to the loop.
	l := d.loop
	d.runner.OnProgress = func(jobID, opID string) {
		d.broker.Publish(api.Event{Type: "op.progress", JobID: jobID, OpID: opID})
	}
	d.runner.OnDone = func(jobID, opID string, res ops.Result) {
		data, _ := json.Marshal(map[string]any{"status": res.Status})
		d.broker.Publish(api.Event{Type: "op.done", JobID: jobID, OpID: opID, Data: data})
		if err := l.HandleOpDone(jobID, opID, res); err != nil {
			log.Printf("loopd: HandleOpDone(%s, %s): %v", jobID, opID, err)
		}
	}

	srv := &http.Server{Addr: listen, Handler: d.handler(token)}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listen, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("loopd: serve: %v", err)
		}
	}()
	log.Printf("loopd listening on %s", listen)

	// The scheduler: one tick at a time. When Step reports no work it
	// sleeps the idle the loop has measured (floored so a fresh daemon
	// does not busy-spin), and it stops starting ticks once the drain
	// has begun — a tick already running finishes and checkpoints
	// (ELR-97).
	var tickRunning atomic.Bool
	go func() {
		for {
			if ctx.Err() != nil || d.drainer.Draining() {
				return
			}
			tickRunning.Store(true)
			ran, err := l.Step(ctx)
			tickRunning.Store(false)
			if err != nil {
				log.Printf("loopd: step: %v", err)
			}
			if ran {
				d.broker.Publish(api.Event{Type: "tick"}) // end of the tick.
				continue
			}
			s := l.IdleWhileReady()
			if s < pollFloor {
				s = pollFloor
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s):
			}
		}
	}()

	<-ctx.Done()

	// SIGTERM: stop taking new work, then wait for the running tick and
	// its ops to finish within the grace period.
	d.drainer.Begin()
	gctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Loopd.ShutdownGraceSeconds)*time.Second)
	left := d.drainer.Wait(gctx, func() []string {
		running := d.runner.Running()
		if tickRunning.Load() {
			running = append([]string{"tick"}, running...)
		}
		return running
	})
	cancel()
	if len(left) > 0 {
		log.Printf("loopd: shutdown deadline reached; still running: %v", left)
	}

	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("loopd: server shutdown: %v", err)
	}
	scancel()
	d.broker.Close()
	return nil
}

// handler is the daemon's root handler: the API's guarded mux, plus the
// loop's private /metrics registry (the skeleton's route, kept).
func (d deps) handler(token string) http.Handler {
	root := http.NewServeMux()
	root.Handle("GET /metrics", promhttp.HandlerFor(d.metrics, promhttp.HandlerOpts{}))
	root.Handle("/", api.New(api.Deps{
		Store:     d.store,
		Runner:    d.runner,
		Redactor:  d.redactor,
		Token:     token,
		Version:   version,
		Submit:    api.NewSubmit(d.store, workspaceManager(), d.loop.Submit),
		Submitter: d.loop.Submit,
		Broker:    d.broker,
		Drainer:   d.drainer,
	}).Handler())
	return root
}

// workspaceManager builds the task worktree manager (card ELR-104,
// M4.2d): the worktrees live under the job store — the one directory
// the container mounts, so they survive a restart — and are cut from
// the repo at $LOOPD_REPO.
func workspaceManager() *workspace.Manager {
	return &workspace.Manager{
		Repo: os.Getenv("LOOPD_REPO"),
		Root: filepath.Join(agentjob.DataDir(), "workspaces"),
	}
}

// storeJobs is the slot.Jobs view over the agent job store: the slot
// manager reads a job's recorded slot state and writes back the warm slot
// and the snapshot it assigns (card ELR-34; the same adapter loopctl
// uses).
type storeJobs struct{ store *agentjob.Store }

// Get returns the job's recorded slot state.
func (j storeJobs) Get(id string) (slot.JobState, error) {
	job, err := j.store.Get(id)
	if err != nil {
		return slot.JobState{}, err
	}
	js := slot.JobState{
		ID:      id,
		Running: job.Status == agentjob.Running,
		Tokens:  job.Tokens,
	}
	if job.Snapshot != nil {
		js.Snapshot = job.Snapshot.File
		js.SnapshotFingerprint = job.Snapshot.Fingerprint
	}
	return js, nil
}

// SetSnapshot records the snapshot the job's cache was saved to.
func (j storeJobs) SetSnapshot(id, file, fingerprint string, tokens int) error {
	job, err := j.store.Get(id)
	if err != nil {
		return err
	}
	job.Snapshot = &agentjob.SnapshotRef{File: file, Fingerprint: fingerprint, Tokens: tokens}
	job.Tokens = tokens
	return j.store.SetStatus(id, job.State)
}

// SetWarmSlot records the slot the job's cache is warm in; nil clears it.
func (j storeJobs) SetWarmSlot(id string, s *int) error {
	job, err := j.store.Get(id)
	if err != nil {
		return err
	}
	job.WarmSlot = s
	return j.store.SetStatus(id, job.State)
}
