package main

// loopctl run (ELR-34, M3.6a): run jobs through the loop by hand against
// the configured engine, without the daemon. It is the live Gate A check:
// the same wiring the daemon will use (engine, slot manager, permission
// guard, tool registry, op runner, loop), driven from the command line.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/slot"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/tools"
)

// run is the body of the "run" subcommand: args starts with "run", the
// rest are flags and one or more instructions. eng is the model backend
// (main builds the llama.cpp client; tests pass the fake engine), out
// receives the report. It returns the process exit code: 0 when every
// job is done, 1 when any job failed or anything else went wrong, 2 when
// a local (Claude Code) round owns the work slot.
func run(args []string, eng engine.Engine, out io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(out, "loopctl: expected the \"run\" subcommand")
		return 1
	}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, `usage: loopctl run [--slot N] [--workspace DIR] "<instructions>" ["<instructions>" ...]

Runs the given jobs through the loop against the configured engine,
without the daemon. Refuses to start while a local round is running.
--slot       engine slot for the jobs (default: the config's slots)
--workspace  workspace directory for the jobs (default: a new directory under the temp dir)
`)
	}
	slotN := fs.Int("slot", -1, "engine slot for the jobs (default: the config's slots)")
	workspace := fs.String("workspace", "", "workspace directory for the jobs (default: a new directory under the temp dir)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	instructions := fs.Args()
	if len(instructions) == 0 {
		fs.Usage()
		return 1
	}

	cfg, err := config.Load(configPath())
	if err != nil {
		fmt.Fprintf(out, "loopctl: %v\n", err)
		return 1
	}

	// The store the daemon's jobs live in: the same directory cmd/jarvis-jobs
	// opens (agentjob.DataDir).
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		fmt.Fprintf(out, "loopctl: %v\n", err)
		return 1
	}

	// The work slot is single: while a local (Claude Code) round is running,
	// the box's agent owns the machine — refuse rather than fight it.
	if jobs, err := store.List(); err != nil {
		fmt.Fprintf(out, "loopctl: list jobs: %v\n", err)
		return 1
	} else {
		for _, j := range jobs {
			if j.Status == agentjob.Running && (j.Driver == "" || j.Driver == "local") {
				fmt.Fprintf(out, "loopctl: refusing to start: job %s is running with the local driver — a Claude Code round owns the work slot\n", j.ID)
				return 2
			}
		}
	}

	ws := *workspace
	if ws == "" {
		ws, err = os.MkdirTemp("", "loopctl-")
		if err != nil {
			fmt.Fprintf(out, "loopctl: %v\n", err)
			return 1
		}
	} else if err := os.MkdirAll(ws, 0o755); err != nil {
		fmt.Fprintf(out, "loopctl: %v\n", err)
		return 1
	}

	// One job per instructions argument.
	unix := time.Now().Unix()
	ids := make([]string, 0, len(instructions))
	for i, ins := range instructions {
		id := fmt.Sprintf("loopctl-%d-%d", unix, i+1)
		added, err := store.Ingest(agentjob.Spec{
			ID:           id,
			Title:        jobTitle(ins),
			Instructions: ins,
			Driver:       "loop",
			Class:        agentjob.ClassBackground,
			Tags:         []string{"loopctl"},
			Workspace:    ws,
		})
		if err != nil {
			fmt.Fprintf(out, "loopctl: ingest %s: %v\n", id, err)
			return 1
		}
		if !added {
			fmt.Fprintf(out, "loopctl: job %s already exists in the store; refusing to overwrite it\n", id)
			return 1
		}
		ids = append(ids, id)
	}

	// The loop's collaborators: the slot manager over the chosen slot(s),
	// the permission guard for the workspace, the registry of the core
	// tools, the runner for the tool calls (its OnDone feeds results back
	// to the loop), and the loop itself.
	slots := cfg.Slots
	if *slotN >= 0 {
		slots = []int{*slotN}
	}
	mgr := slot.New(
		slot.Config{Slots: slots, SnapshotMinTokens: cfg.SnapshotMinTokens},
		eng,
		storeJobs{store: store},
	)

	guard := &perm.Guard{WorkDir: ws}
	toolList := make([]ops.Tool, 0, 8)
	toolList = append(toolList, tools.FileTools(guard)...)
	toolList = append(toolList, tools.ShellTool(guard))
	toolList = append(toolList, tools.FetchTool())
	// The spill store is the ops directory under each job dir: a spilled
	// handle is the path of the full text, so reading it back is a ReadFile.
	toolList = append(toolList, tools.SearchOutput{Open: func(handle string) (string, error) {
		b, err := os.ReadFile(handle)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}})
	reg := ops.NewRegistry(toolList...)

	var l *loop.Loop
	runner := &ops.Runner{
		Registry: reg,
		Timeouts: map[ops.Class]time.Duration{
			ops.Quick:  time.Duration(cfg.Timeouts.QuickSeconds) * time.Second,
			ops.Normal: time.Duration(cfg.Timeouts.NormalSeconds) * time.Second,
		},
		OnDone: func(jobID, opID string, res ops.Result) {
			if err := l.HandleOpDone(jobID, opID, res); err != nil {
				fmt.Fprintf(out, "loopctl: HandleOpDone(%s, %s): %v\n", jobID, opID, err)
			}
		},
	}
	l = loop.New(loop.Deps{
		Engine:   eng,
		Slots:    mgr,
		Runner:   runner,
		Registry: reg,
		Store:    store,
		Config:   cfg,
		Now:      time.Now,
	})

	for _, id := range ids {
		if err := l.Submit(id); err != nil {
			fmt.Fprintf(out, "loopctl: submit %s: %v\n", id, err)
			return 1
		}
	}

	ctx := context.Background()
	start := time.Now()
	own := make(map[string]bool, len(ids))
	for _, id := range ids {
		own[id] = true
	}
	for {
		terminal, err := allTerminal(store, own)
		if err != nil {
			fmt.Fprintf(out, "loopctl: %v\n", err)
			return 1
		}
		if terminal {
			break
		}
		ran, err := l.Step(ctx)
		if err != nil {
			fmt.Fprintf(out, "loopctl: step: %v\n", err)
			return 1
		}
		if !ran {
			time.Sleep(200 * time.Millisecond)
		}
	}

	anyFailed := false
	for _, id := range ids {
		j, err := store.Get(id)
		if err != nil {
			fmt.Fprintf(out, "loopctl: %s: %v\n", id, err)
			return 1
		}
		msgs, err := store.Messages(id)
		if err != nil {
			fmt.Fprintf(out, "loopctl: %s: %v\n", id, err)
			return 1
		}
		ticks := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				ticks++
			}
		}
		if j.Status == agentjob.Failed {
			anyFailed = true
		}
		fmt.Fprintf(out, "%s  status=%s  ticks=%d  summary=%s\n", id, j.Status, ticks, j.Summary)
	}
	fmt.Fprintf(out, "idle_while_ready_seconds: %d\n", int(l.IdleWhileReady().Seconds()))
	fmt.Fprintf(out, "wall_seconds: %d\n", int(time.Since(start).Seconds()))
	if anyFailed {
		return 1
	}
	return 0
}

// allTerminal reports whether every job of this run is done or failed.
func allTerminal(store *agentjob.Store, own map[string]bool) (bool, error) {
	jobs, err := store.List()
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if !own[j.ID] {
			continue
		}
		if j.Status != agentjob.Done && j.Status != agentjob.Failed {
			return false, nil
		}
	}
	return true, nil
}

// jobTitle is the first line of the instructions, cut to 80 chars.
func jobTitle(ins string) string {
	s := strings.TrimSpace(ins)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	if s == "" {
		s = "loopctl job"
	}
	return s
}

// storeJobs is the slot.Jobs view over the agent job store: the slot
// manager reads a job's recorded slot state and writes back the warm slot
// and the snapshot it assigns (card ELR-34).
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
