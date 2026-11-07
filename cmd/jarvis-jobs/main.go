// jarvis-jobs is how the agent-run loop works with queued agent jobs.
//
//	jarvis-jobs list [status]              all jobs, or only one status (e.g. failed)
//	jarvis-jobs next                       print "<id>\t<title>" of the next
//	                                       ready job whose engine is up; a
//	                                       job whose driver's engine is down
//	                                       is skipped (left ready), so the
//	                                       worker idles through a brain-swap
//	                                       window (docs/brain-swap.md)
//	jarvis-jobs photo-ready                comma-separated ids of the ready
//	                                       photo-tagged batch — the set the
//	                                       agent-run watcher brackets with one
//	                                       brain-swap window (empty when none)
//	jarvis-jobs run <id> [-driver local|igpu|sonnet|superagent] [-round N] [-note TEXT]
//	                   [-auto]             run under the NoBrakes guard:
//	                                       take the auto-run marker (at most
//	                                       one concurrent auto job) and exit 3
//	                                       while another auto job holds it
//	jarvis-jobs autopause <id>             an auto job failed: pause
//	                                       autonomous execution (auto_mode +
//	                                       full_auto off) and DM the owner the
//	                                       job and the error
//	jarvis-jobs running [clear]            show (or clear) the id of the auto
//	                                       job that holds the running marker
//	jarvis-jobs finish <id> <exit-code>    record a round that ran elsewhere (after a reboot)
//	jarvis-jobs stop <id>            stop a running job: kill the round
//	                               and park the job out of the queue — the
//	                               loop skips to the next work
//	jarvis-jobs skip <id> | requeue [-driver local|igpu|sonnet|superagent]
//	                   [-class fix|build|background] [-priority N] <id> | show <id>
//	jarvis-jobs add -id ID -title T [-driver local|igpu|sonnet|superagent] [-source S]
//	                   [-class fix|build|background] [-priority N] [-depends a,b]
//	                   [-tags a,b] [-kanban-task TASKID]
//	jarvis-jobs nightly            NoBrakes: read the Redis auto_mode flag;
//	                               if on and today's nightly window is
//	                               unprocessed, enqueue parked nightly jobs
//	jarvis-jobs fullauto [on|off]  NoBrakes: read (prints "on"/"off") or
//	                               set the full-auto flag — on = the loop
//	                               runs every ready job without asking
//	jarvis-jobs automode [on|off]  NoBrakes: read or set the auto_mode flag
//	                               (the nightly-batch switch)
//	jarvis-jobs keepgoing [on|off] NoBrakes: read or set the keep-going
//	                               flag — on = a failed auto job notifies
//	                               but does not pause the loop
//	jarvis-jobs failnote <id>      NoBrakes: an auto job failed under
//	                               keep-going: DM the owner the job and the
//	                               error WITHOUT pausing (no flag writes)
//	jarvis-jobs audit [n]          NoBrakes: show the last n entries of the
//	                               auto-execution audit log (default: all);
//	                               -json prints the raw JSON lines
//	jarvis-jobs enqueue <id>       move one parked job to ready (manual)
//	jarvis-jobs order [a,b,c]    show or set the runtime band priority order
//	                              (a permutation of fix, build, background)
//	jarvis-jobs message [TEXT]   send the agent a message (stdin when TEXT
//	                              is "-" or absent); injected at its next step
//	jarvis-jobs restart [--now]  restart the agent-run service (deferred
//	                              until the current round ends unless --now)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/power"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "jarvis-jobs: "+format+"\n", a...)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: jarvis-jobs list [status]|next|photo-ready|run|finish|restart|stop|skip|requeue|show|pause|add|order|message|nightly|audit|fullauto|automode|keepgoing|autopause|failnote|running|enqueue")
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		die("%v", err)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "add":
		if err := cmdAdd(store, args, os.Stdin); err != nil {
			die("%v", err)
		}
	case "list":
		if err := cmdList(store, args, os.Stdout); err != nil {
			die("%v", err)
		}
	case "next":
		j, err := nextRunnable(store)
		if err != nil {
			die("%v", err)
		}
		if j == nil {
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\n", j.ID, j.Title)
	case "photo-ready":
		jobs, err := store.PhotoReady()
		if err != nil {
			die("%v", err)
		}
		ids := make([]string, 0, len(jobs))
		for _, j := range jobs {
			ids = append(ids, j.ID)
		}
		// The agent-run watcher reads this: one line, comma-separated ids,
		// empty line when the photo batch is empty.
		fmt.Println(strings.Join(ids, ","))
	case "show":
		if len(args) != 1 {
			die("show <id>")
		}
		j, err := store.Get(args[0])
		if err != nil {
			die("%v", err)
		}
		tags := ""
		if len(j.Tags) > 0 {
			tags = fmt.Sprintf("\ntags: %s", strings.Join(j.Tags, ", "))
		}
		// The driver the loop should run this job on: the job's own spec
		// driver when it names a valid one (e.g. a sonnet-gated job), the
		// default "local" otherwise. The agent-run full-auto branch reads
		// this line (see `fullauto` above).
		driver := j.Driver
		switch driver {
		case "local", "igpu", "sonnet", "superagent":
		default:
			driver = "local"
		}
		fmt.Printf("%s — %s\nstatus: %s\nclass: %s\ndriver: %s\neffort: %s%s\n\n%s\n", j.ID, j.Title, j.Status,
			agentjob.EffectiveClass(j.Class, j.Source), driver, queue.CanonicalEffort(j.ReasoningEffort, "coding"), tags, j.Instructions)
	case "pause":
		if len(args) != 1 {
			die("pause <id>")
		}
		if err := store.Pause(args[0]); err != nil {
			die("%v", err)
		}
	case "stop":
		if len(args) != 1 {
			die("stop <id>")
		}
		if err := stopJob(store, args[0]); err != nil {
			die("%v", err)
		}
	case "skip":
		if len(args) != 1 {
			die("skip <id>")
		}
		if err := store.SetStatus(args[0], agentjob.State{Status: agentjob.Skipped}); err != nil {
			die("%v", err)
		}
	case "requeue":
		if err := cmdRequeue(store, args); err != nil {
			die("%v", err)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		driver := fs.String("driver", "local", "local, igpu, sonnet or superagent")
		round := fs.String("round", "", "round label")
		note := fs.String("note", "", "typed instructions for this round")
		effort := fs.String("effort", "",
			"reasoning effort for this round: off, low, medium, high, xhigh, max (default: job spec, else medium)")
		auto := fs.Bool("auto", false,
			"NoBrakes: run under the auto-execution guard (take the running marker; exit 3 while another auto job holds it)")
		if len(args) < 1 {
			die("run <id> [flags]")
		}
		fs.Parse(args[1:])
		// Only overwrite the stored job effort when the flag is passed.
		if *effort != "" {
			j, err := store.Get(args[0])
			if err == nil {
				j.ReasoningEffort = queue.CanonicalEffort(*effort, "coding")
				store.Ingest(j.Spec)
			}
		}
		if *auto {
			if err := runAuto(store, args[0], *driver, *round, *note); err != nil {
				if errors.Is(err, errAutoHeld) {
					fmt.Fprintf(os.Stderr, "jarvis-jobs: run -auto %s: %v\n", args[0], err)
					os.Exit(3)
				}
				die("%v", err)
			}
			return
		}
		r := runner(store)
		r.Tee = os.Stdout
		j, err := r.Run(context.Background(), args[0], *round, *driver, *note)
		fmt.Printf("\n==> job %s %s %s\n", j.ID, j.Status, j.PRURL)
		if err != nil {
			os.Exit(1)
		}
	case "finish":
		if len(args) != 2 {
			die("finish <id> <exit-code>")
		}
		var code int
		fmt.Sscan(args[1], &code)
		j, _ := runner(store).Finish(args[0], code, "")
		fmt.Printf("==> job %s %s %s\n", j.ID, j.Status, j.PRURL)
	case "nightly":
		if err := cmdNightly(store); err != nil {
			die("%v", err)
		}
	case "fullauto":
		if err := cmdFullAuto(os.Stdout, args); err != nil {
			die("%v", err)
		}
	case "automode":
		if err := cmdAutoMode(os.Stdout, args); err != nil {
			die("%v", err)
		}
	case "keepgoing":
		if err := cmdKeepGoing(os.Stdout, args); err != nil {
			die("%v", err)
		}
	case "autopause":
		if err := cmdAutoPause(store, args, discordNotify); err != nil {
			die("%v", err)
		}
	case "failnote":
		if err := cmdFailNote(store, args, discordNotify); err != nil {
			die("%v", err)
		}
	case "running":
		if err := cmdRunning(os.Stdout, args); err != nil {
			die("%v", err)
		}
	case "audit":
		if err := cmdAudit(args); err != nil {
			die("%v", err)
		}
	case "enqueue":
		if len(args) != 1 {
			die("enqueue <id>")
		}
		if err := store.Enqueue(args[0]); err != nil {
			die("%v", err)
		}
		fmt.Println(args[0])
	case "order":
		if err := cmdOrder(store, args); err != nil {
			die("%v", err)
		}
	case "message":
		if err := cmdMessage(args, os.Stdin); err != nil {
			die("%v", err)
		}
	case "restart":
		if err := cmdRestart(args); err != nil {
			die("%v", err)
		}
	default:
		die("unknown command %q", cmd)
	}
}

// cmdMessage sends a message to the agent's inbox (the queue site does the
// same via POST /api/owner-message). Text is one or more arguments, or
// stdin when the argument is "-" or absent.
func cmdMessage(args []string, stdin io.Reader) error {
	var text string
	switch {
	case len(args) == 0:
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	case args[0] == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		text = string(b)
	default:
		text = strings.Join(args, " ")
	}
	name, err := inbox.Send(text)
	if err != nil {
		return err
	}
	fmt.Printf("queued %s (%d waiting)\n", name, inbox.Count())
	return nil
}

// cmdNightly runs the NoBrakes nightly gate once (NoBrakes 2/6); the
// agent-run loop calls it at the top of every iteration. When the Redis
// auto_mode flag is "on" and today's UTC nightly window has not been
// processed yet, it enqueues every parked nightly-tagged job (no manual
// trigger) and stamps the window so the batch runs once per day.
//
// It never kills the loop: with Redis disabled ($JARVIS_REDIS=off) there
// is nothing to read and it exits 0 quietly; with Redis unreachable (or
// the gate failing mid-run) it logs a warning and exits 0 too — the
// feature fails safe to off. Only a store it cannot open is a hard error.
func cmdNightly(store *agentjob.Store) error {
	addr := agentjob.StreamAddr()
	if addr == "off" || addr == "none" {
		return nil // no Redis configured: nothing to read, off
	}
	flags, err := nobrakes.New(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes nightly: redis %s unreachable (%v) — treating auto_mode as off\n", addr, err)
		return nil
	}
	defer flags.Close()
	// NoBrakes 4/6: the kanban board is the canonical source of the nightly
	// designation for jobs linked to a task. If the board cannot be read we
	// pass nil and the gate falls back to the job-level nightly tag.
	board, err := kanban.Open(agentjob.DataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes nightly: kanban store: %v — falling back to job tags\n", err)
		board = nil
	}
	// NoBrakes 5/6: every auto-enqueued job lands in the durable audit log.
	// The log is a record, not the gate: if it cannot be opened the pass
	// runs without it (the enqueue line below is still in the loop log).
	var audit nobrakes.AuditRecorder
	auditLog, err := nobrakes.NewAuditLog(nobrakes.DefaultAuditPath(agentjob.DataDir()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes nightly: audit log: %v — pass runs unaudited\n", err)
	} else {
		audit = auditLog
		defer auditLog.Close()
	}
	enqueued, _, err := nobrakes.AutoEnqueue(context.Background(), flags, store, board, time.Now(), audit)
	if err != nil {
		if len(enqueued) > 0 {
			// The gate acted (jobs enqueued, window stamped): the error is
			// about the record, not the switch.
			fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes nightly: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes nightly: %v — treating auto_mode as off\n", err)
		}
		return nil
	}
	if len(enqueued) > 0 {
		fmt.Printf("jarvis-jobs: nobrakes nightly: auto-enqueued %d nightly job(s): %s\n",
			len(enqueued), strings.Join(enqueued, " "))
	}
	// Steady state (off, or window already stamped) prints nothing: the
	// loop log is for action and failure, not for every iteration.
	return nil
}

// cmdFlag is the shared get/set handler for the NoBrakes flag switches
// (fullauto, automode, keepgoing): `jarvis-jobs <switch>` prints "on" or
// "off"; `jarvis-jobs <switch> on|off` sets the flag and prints the stored
// state. name is the human name of the switch for the messages.
//
// A get never fails the caller: with Redis disabled ($JARVIS_REDIS=off),
// unreachable, or holding a corrupt value it prints "off" (and a warning
// on stderr) and returns nil — the agent-run loop relies on that to fail
// safe on every iteration. A set that cannot reach Redis returns an error
// instead: an operator must never believe a switch they flipped is on.
func cmdFlag(w io.Writer, args []string, name string,
	get func(ctx context.Context, st *nobrakes.Store) (string, error),
	set func(ctx context.Context, st *nobrakes.Store, on bool) error) error {
	setFlag := false
	switch len(args) {
	case 0:
		// get
	case 1:
		switch strings.ToLower(args[0]) {
		case "on", "off":
			setFlag = true
		default:
			return fmt.Errorf("bad value %q (want on or off)", args[0])
		}
	default:
		return fmt.Errorf("usage: jarvis-jobs %s [on|off]", name)
	}
	addr := agentjob.StreamAddr()
	if addr == "off" || addr == "none" {
		if !setFlag {
			fmt.Fprintln(w, "off")
			return nil
		}
		return fmt.Errorf("NoBrakes is disabled here (JARVIS_REDIS is off), so there is no %s flag to set", name)
	}
	st, err := nobrakes.New(addr)
	if err != nil {
		if !setFlag {
			fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes %s: redis %s unreachable (%v) — treating %s as off\n", name, addr, err, name)
			fmt.Fprintln(w, "off")
			return nil
		}
		return fmt.Errorf("nobrakes %s: redis %s unreachable: %w", name, addr, err)
	}
	defer st.Close()
	ctx := context.Background()
	if setFlag {
		if err := set(ctx, st, strings.ToLower(args[0]) == "on"); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	v, err := get(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: nobrakes %s: read: %v — treating %s as off\n", name, err, name)
		fmt.Fprintln(w, "off")
		return nil
	}
	fmt.Fprintln(w, v)
	return nil
}

// cmdFullAuto reads or sets the NoBrakes full-auto flag
// (nobrakes:full_auto): `jarvis-jobs fullauto [on|off]`.
func cmdFullAuto(w io.Writer, args []string) error {
	return cmdFlag(w, args, "full auto",
		func(ctx context.Context, st *nobrakes.Store) (string, error) { return st.GetFullAuto(ctx) },
		func(ctx context.Context, st *nobrakes.Store, on bool) error { return st.SetFullAuto(ctx, on) })
}

// cmdAutoMode reads or sets the NoBrakes auto_mode flag
// (nobrakes:auto_mode): `jarvis-jobs automode [on|off]`. The nightly gate
// (`jarvis-jobs nightly`) is the only reader of this flag in the loop.
func cmdAutoMode(w io.Writer, args []string) error {
	return cmdFlag(w, args, "auto mode",
		func(ctx context.Context, st *nobrakes.Store) (string, error) { return st.GetAutoMode(ctx) },
		func(ctx context.Context, st *nobrakes.Store, on bool) error { return st.SetAutoMode(ctx, on) })
}

// cmdKeepGoing reads or sets the NoBrakes keep-going flag
// (nobrakes:keep_going): `jarvis-jobs keepgoing [on|off]`. When on, a
// failed auto job notifies the owner without pausing autonomous execution.
func cmdKeepGoing(w io.Writer, args []string) error {
	return cmdFlag(w, args, "keep going",
		func(ctx context.Context, st *nobrakes.Store) (string, error) { return st.GetKeepGoing(ctx) },
		func(ctx context.Context, st *nobrakes.Store, on bool) error { return st.SetKeepGoing(ctx, on) })
}

// errAutoHeld marks the "another auto job is running" outcome of
// run -auto. The caller maps it to exit 3 — distinct from die()'s 2 and
// from a failed job's exit 1 — so the agent-run loop can tell a busy
// marker (idle-poll and retry) apart from a run failure.
var errAutoHeld = errors.New("another auto job is running")

// runAuto runs a job under the NoBrakes auto-execution guard (6/6): it
// takes the nobrakes:running marker — at most one auto job at a time —
// and releases it when the round ends, whatever the outcome. A marker
// held by a job that is no longer running is stale and is taken over
// (crash, reboot, a round that never released); a marker held by a live
// job is not. The round itself runs exactly like a manual `run`.
//
// Without a reachable Redis there is no marker to take, so -auto
// refuses to run: an unguarded auto run is exactly what the guard
// exists to prevent.
func runAuto(store *agentjob.Store, id, driver, round, note string) error {
	addr := agentjob.StreamAddr()
	if addr == "off" || addr == "none" {
		return fmt.Errorf("NoBrakes is disabled here (JARVIS_REDIS is off), so there is no auto-run marker — run the job manually")
	}
	flags, err := nobrakes.New(addr)
	if err != nil {
		return fmt.Errorf("nobrakes: redis %s unreachable: %w", addr, err)
	}
	defer flags.Close()
	ctx := context.Background()
	// A holder whose job is gone from the store or no longer "running"
	// left a stale marker: take it over rather than idle-polling forever.
	alive := func(holder string) bool {
		j, err := store.Get(holder)
		return err == nil && j.Status == agentjob.Running
	}
	holder, ok, err := flags.AcquireRunning(ctx, id, alive)
	if err != nil {
		return fmt.Errorf("acquire %s: %w", nobrakes.RunningKey, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", errAutoHeld, holder)
	}
	defer func() {
		if err := flags.ReleaseRunning(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "jarvis-jobs: release %s: %v (a stale marker is taken over by the next auto round)\n", nobrakes.RunningKey, err)
		}
	}()
	r := runner(store)
	r.Tee = os.Stdout
	j, err := r.Run(ctx, id, round, driver, note)
	fmt.Printf("\n==> job %s %s %s\n", j.ID, j.Status, j.PRURL)
	return err
}

// failureReason pulls the human reason out of a finished job: the stored
// error, or a fallback when the store has none.
func failureReason(j agentjob.Job) string {
	reason := strings.TrimSpace(j.Error)
	if reason == "" {
		reason = "the round exited non-zero"
	}
	return reason
}

// autoFailureMessage is the shared first half of the owner-facing
// notification for a failed auto job: which job, and why.
func autoFailureMessage(j agentjob.Job) string {
	return fmt.Sprintf("NoBrakes: auto job %q (%s) failed: %s", j.Title, j.ID, failureReason(j))
}

// cmdAutoPause is the NoBrakes error path (6/6): an auto-executed job
// failed, so autonomous execution is paused — auto_mode and full_auto
// are both set off — and the owner is told on Discord which job failed
// and why. The pause write is best-effort: a missing or corrupt flag
// already reads as off in the loop, so even with a dead Redis the system
// ends up safe, and the notification goes out either way — the owner
// must hear about the failure. Re-enabling is a deliberate operator act
// (!automode on / !fullauto on); the feature never re-enables itself.
func cmdAutoPause(store *agentjob.Store, args []string, notify func(string) error) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jarvis-jobs autopause <id>")
	}
	j, err := store.Get(args[0])
	if err != nil {
		return err
	}
	reason := failureReason(j)
	if addr := agentjob.StreamAddr(); addr == "off" || addr == "none" {
		fmt.Fprintln(os.Stderr, "jarvis-jobs: autopause: NoBrakes disabled (JARVIS_REDIS off) — nothing to pause")
	} else if flags, err := nobrakes.New(addr); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: autopause: redis %s unreachable (%v) — flags left as-is (they read off anyway)\n", addr, err)
	} else {
		defer flags.Close()
		if err := flags.Pause(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "jarvis-jobs: autopause: pause: %v — flags left as-is (they read off anyway)\n", err)
		}
	}
	msg := autoFailureMessage(j) +
		" Autonomous execution is paused — auto_mode and full_auto are now off; re-enable with !automode on / !fullauto on."
	if err := notify(msg); err != nil {
		return fmt.Errorf("discord notify: %w", err)
	}
	fmt.Printf("auto mode paused: %s (%s): %s\n", j.ID, j.Title, reason)
	return nil
}

// cmdFailNote is the keep-going failure path: an auto-executed job failed
// but keep_going is on, so autonomous execution is NOT paused — the owner
// is still told on Discord which job failed and why. It never touches a
// flag. The failed job stays "failed" in the store (it is not re-picked);
// the loop moves on to the next ready job. The notification goes out even
// with a dead Redis — the owner must hear about the failure.
func cmdFailNote(store *agentjob.Store, args []string, notify func(string) error) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: jarvis-jobs failnote <id>")
	}
	j, err := store.Get(args[0])
	if err != nil {
		return err
	}
	msg := autoFailureMessage(j) +
		" Keep going is on, so autonomous execution continues with the next ready job — say !keepgoing off (or !fullauto off) to stop."
	if err := notify(msg); err != nil {
		return fmt.Errorf("discord notify: %w", err)
	}
	fmt.Printf("keep going: %s (%s) failed: %s — not pausing\n", j.ID, j.Title, failureReason(j))
	return nil
}

// discordNotify DMs the owner through the jarvis-discord helper — the
// same transport the agent-run loop's notify() uses.
func discordNotify(msg string) error {
	cmd := exec.Command("jarvis-discord", "notify", msg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdRunning shows or clears the NoBrakes auto-run marker (6/6).
// `jarvis-jobs running` prints the id of the auto job that holds
// nobrakes:running, or "(none)". `jarvis-jobs running clear` removes it —
// the operator's manual override for a marker left behind by a crash
// (a stale marker is also taken over automatically once the holder's
// job is no longer running).
func cmdRunning(w io.Writer, args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "clear") {
		return fmt.Errorf("usage: jarvis-jobs running [clear]")
	}
	addr := agentjob.StreamAddr()
	if addr == "off" || addr == "none" {
		fmt.Fprintln(w, "(none) — NoBrakes disabled (JARVIS_REDIS off)")
		return nil
	}
	flags, err := nobrakes.New(addr)
	if err != nil {
		return fmt.Errorf("nobrakes: redis %s unreachable: %w", addr, err)
	}
	defer flags.Close()
	ctx := context.Background()
	if len(args) == 1 {
		if err := flags.ClearRunning(ctx); err != nil {
			return err
		}
		fmt.Fprintln(w, "cleared")
		return nil
	}
	holder, err := flags.RunningHolder(ctx)
	if err != nil {
		return err
	}
	if holder == "" {
		fmt.Fprintln(w, "(none)")
		return nil
	}
	fmt.Fprintln(w, holder)
	return nil
}

// cmdAudit shows the NoBrakes audit log (5/6): every job the nightly gate
// auto-enqueued — timestamp, nightly window, job id/title, trigger — most
// recent last. `audit [n]` shows the last n entries (default: all);
// -json prints the raw JSON lines for scripting.
func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print the raw JSON lines")
	fs.Parse(args)
	args = fs.Args()
	n := 0
	if len(args) == 1 {
		if _, err := fmt.Sscanf(args[0], "%d", &n); err != nil {
			die("audit [n]")
		}
	}
	entries, err := nobrakes.RecentAudit(nobrakes.DefaultAuditPath(agentjob.DataDir()), n)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}
	for _, e := range entries {
		fmt.Printf("%s  %s  %s  (window %s, %s)\n", e.Timestamp, e.JobID, e.Title, e.Window, e.Trigger)
	}
	return nil
}

// cmdRestart restarts the agent-run service through the agent-run-restart
// helper (scripts/agent-run-restart, installed to /usr/local/bin by
// install_agent_run.sh). The helper defers the restart until the current
// round ends unless --now is passed, and starts the service when it is
// down, so this works from any shell even while no round process is alive.
func cmdRestart(args []string) error {
	const helper = "/usr/local/bin/agent-run-restart"
	if _, err := os.Stat(helper); err != nil {
		return fmt.Errorf("helper not installed at %s — run scripts/install_agent_run.sh", helper)
	}
	cmd := exec.Command(helper, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdOrder shows or sets the runtime band priority order (highest first).
// `jarvis-jobs order` prints the current order; `jarvis-jobs order
// build,fix,background` sets it. The dashboard's queue settings do the same
// through POST /api/queue/band-order; both persist to the job store.
func cmdOrder(store *agentjob.Store, args []string) error {
	switch len(args) {
	case 0:
		order, err := store.BandOrder()
		if err != nil {
			return err
		}
		fmt.Printf("band order: %s\n", strings.Join(order, " -> "))
		return nil
	case 1:
		order := strings.Split(args[0], ",")
		for i := range order {
			order[i] = strings.TrimSpace(order[i])
		}
		if err := store.SetBandOrder(order); err != nil {
			return err
		}
		fmt.Printf("band order set: %s\n", strings.Join(order, " -> "))
		return nil
	default:
		return fmt.Errorf("usage: jarvis-jobs order [fix,build,background]")
	}
}

func runner(store *agentjob.Store) *agentjob.Runner {
	h, err := queue.NewHistory()
	if err != nil {
		die("history: %v", err)
	}
	r := &agentjob.Runner{Store: store, History: h}
	if conf := confDir(); conf != "" {
		r.SignalFile = conf + "/pause-signal"
		// !restart DM (jarvis-chat) writes this; the signal watcher kills
		// the round and re-queues the job for a fresh run.
		r.RestartSignalFile = conf + "/restart-signal"
		// Stop button (dashboard) or `jarvis-jobs stop` writes this; the
		// signal watcher kills the round and parks the job so the loop
		// skips to the next work.
		r.StopSignalFile = conf + "/stop-signal"
	}
	// Sample GPU power for the whole round; the total lands in the job
	// state and history as kWh + $ at the configured JARVIS_POWER_RATE.
	r.Power = func(jobDir string) func() float64 {
		s := power.NewSampler(jobDir)
		s.Start()
		return s.Stop
	}
	return r
}

// confDir returns JARVIS_CONF, defaulting to ~/.config/jarvis — the same
// directory jarvis-chat and the dashboard use for signal files. The runner
// only polled signal files when JARVIS_CONF was set, which it is not in
// production, so !pause / !restart / stop were silently dead before.
func confDir() string {
	if d := os.Getenv("JARVIS_CONF"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "jarvis")
}

// nextRunnable returns the next ready job whose driver can actually run
// right now: the engine the driver runs on must answer /health, otherwise
// the job is skipped (left ready) and the next one is tried. This is what
// lets the worker idle-poll through a brain-swap photo window
// (docs/brain-swap.md) instead of burning local rounds on a brain that is
// deliberately off, and what stops it from doing the same while the
// watchdog is restarting a crashed brain.
func nextRunnable(store *agentjob.Store) (*agentjob.Job, error) {
	exclude := map[string]bool{}
	for {
		j, err := store.NextExcluding(exclude)
		if err != nil || j == nil {
			return j, err
		}
		d := effectiveDriver(j)
		if driverUp(d) {
			return j, nil
		}
		fmt.Fprintf(os.Stderr, "jarvis-jobs: skipping %s — driver %s engine is down (it stays ready)\n", j.ID, d)
		exclude[j.ID] = true
	}
}

// effectiveDriver is the driver scripts/agent-run would run this job on:
// the job's own spec driver when it names one, else the last round's driver
// ($JARVIS_CONF/driver), else local.
func effectiveDriver(j *agentjob.Job) string {
	if j.Driver != "" {
		return j.Driver
	}
	if b, err := os.ReadFile(filepath.Join(confDir(), "driver")); err == nil {
		if d := strings.TrimSpace(string(b)); d != "" {
			return d
		}
	}
	return "local"
}

// driverUp probes the local engine a driver runs on. Drivers with no local
// engine (sonnet, superagent) always count as up.
func driverUp(driver string) bool {
	var url string
	switch driver {
	case "local":
		url = envOr("JARVIS_BRAIN_URL", "http://127.0.0.1:11434")
	case "igpu":
		url = envOr("JARVIS_IGPU_URL", "http://127.0.0.1:11435")
	default:
		return true
	}
	return healthUp(url + "/health")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// healthUp is true when the endpoint answers 200. A 503 (server up but the
// model still loading) counts as down: a round started now would fail.
func healthUp(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cmdRequeue puts a job back in the queue (status ready), optionally
// re-setting its driver, class and priority. The brain-swap window uses it
// to flip queued photo jobs to the iGPU driver and push them to the front
// of their band (docs/brain-swap.md).
func cmdRequeue(store *agentjob.Store, args []string) error {
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	driver := fs.String("driver", "", "re-set the job's driver: local, igpu, sonnet or superagent")
	class := fs.String("class", "", "re-set the job's class: fix, build or background")
	priority := fs.Int("priority", -1, "re-set the job's priority (higher runs first within the class)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("requeue [-driver local|igpu|sonnet|superagent] [-class fix|build|background] [-priority N] <id>")
	}
	id := fs.Arg(0)
	if *driver != "" {
		if err := store.SetDriver(id, *driver); err != nil {
			return err
		}
	}
	if *class != "" || *priority >= 0 {
		switch *class {
		case "", agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassBackground:
		default:
			return fmt.Errorf("bad class %q (fix, build or background)", *class)
		}
		j, err := store.Get(id)
		if err != nil {
			return err
		}
		c, p := *class, *priority
		if c == "" {
			c = j.Class
		}
		if p < 0 {
			p = j.Priority
		}
		if err := store.SetOrder(id, c, p); err != nil {
			return err
		}
	}
	return store.SetStatus(id, agentjob.State{Status: agentjob.Ready})
}

// stopJob stops a running job: it writes the stop-signal file the runner's
// signal watcher polls, then waits for the job to be parked. If the watcher
// never acts (an old runner binary, or the round already ending), it parks
// the job directly so the loop skips it either way.
func stopJob(store *agentjob.Store, id string) error {
	j, err := store.Get(id)
	if err != nil {
		return err
	}
	switch j.Status {
	case agentjob.Parked:
		fmt.Printf("already parked: %s (re-queue with: jarvis-jobs requeue %s)\n", j.ID, j.ID)
		return nil
	case agentjob.Running, agentjob.Claimed:
		// fall through — write the signal
	default:
		return fmt.Errorf("job %s is %s, not running", j.ID, j.Status)
	}
	conf := confDir()
	if conf == "" {
		return errors.New("cannot determine the JARVIS_CONF directory")
	}
	if err := os.WriteFile(conf+"/stop-signal", nil, 0o644); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if j, err := store.Get(id); err == nil && j.Status == agentjob.Parked {
			fmt.Printf("stopped: %s — parked (re-queue with: jarvis-jobs requeue %s)\n", j.ID, j.ID)
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The watcher did not act: park the store entry directly. If the job
	// has finished meanwhile, Stop refuses and the stale signal is cleared.
	if err := store.Stop(id); err != nil {
		os.Remove(conf + "/stop-signal")
		return err
	}
	fmt.Printf("stopped: %s — parked (re-queue with: jarvis-jobs requeue %s)\n", j.ID, j.ID)
	return nil
}

// cmdList prints all jobs in queue order, optionally filtered to one status
// (e.g. `jarvis-jobs list failed`). The trailing "# ..." line reports the
// per-status totals for the *unfiltered* set, so a view truncated by head or
// a pager still reveals how many jobs exist beyond what was shown.
func cmdList(store *agentjob.Store, args []string, w io.Writer) error {
	if err := store.Recover(); err != nil {
		return err
	}
	jobs, err := store.List()
	if err != nil {
		return err
	}
	var filter string
	if len(args) > 0 {
		filter = args[0]
	}
	counts := map[string]int{}
	for _, j := range jobs {
		counts[j.Status]++
	}
	for _, j := range jobs {
		if filter != "" && j.Status != filter {
			continue
		}
		fmt.Fprintf(w, "%-10s %-40s %s %s\n", j.Status, j.ID, j.Title, j.PRURL)
	}
	var parts []string
	for _, st := range []string{agentjob.Ready, agentjob.Parked, agentjob.Claimed,
		agentjob.Running, agentjob.Paused, agentjob.Done, agentjob.Failed, agentjob.Skipped} {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	fmt.Fprintf(w, "# %d jobs: %s\n", len(jobs), strings.Join(parts, ", "))
	return nil
}

func cmdAdd(store *agentjob.Store, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	id := fs.String("id", "", "job id")
	title := fs.String("title", "", "job title")
	driver := fs.String("driver", "local", "local, igpu, sonnet or superagent")
	source := fs.String("source", "", "origin (discord, migrate, …)")
	class := fs.String("class", agentjob.ClassBuild, "fix, build or background (fix runs before build, build before background)")
	priority := fs.Int("priority", 0, "higher runs first within the class")
	instructions := fs.String("instructions", "", "job body (default: stdin)")
	effort := fs.String("effort", "", "reasoning effort: off, low, medium, high, xhigh, max (default medium)")
	depends := fs.String("depends", "", "comma-separated job ids this job waits on (they must be done first)")
	tags := fs.String("tags", "", "comma-separated tags (nightly: stored parked until the NoBrakes nightly gate enqueues it)")
	kanbanTask := fs.String("kanban-task", "", "kanban task id this job was created from (NoBrakes: the task's nightly tag then decides auto-eligibility)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *title == "" {
		return fmt.Errorf("add -id ID -title T")
	}
	var deps []string
	for _, d := range strings.Split(*depends, ",") {
		if d = strings.TrimSpace(d); d != "" {
			deps = append(deps, d)
		}
	}
	var tagList []string
	for _, tg := range strings.Split(*tags, ",") {
		if tg = strings.TrimSpace(tg); tg != "" {
			tagList = append(tagList, tg)
		}
	}
	switch *class {
	case "", agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassBackground:
	default:
		return fmt.Errorf("-class must be fix, build or background (got %q)", *class)
	}
	body := *instructions
	if body == "" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		body = string(b)
	}
	// NoBrakes 4/6: a job linked to a kanban task whose nightly tag is set is
	// a nightly job — tag it so Ingest stores it parked (the nightly gate
	// then re-resolves eligibility from the board, which stays canonical).
	if *kanbanTask != "" && !hasTag(tagList, agentjob.TagNightly) {
		if board, err := kanban.Open(agentjob.DataDir()); err == nil {
			if t, err := board.GetTask(*kanbanTask); err == nil && t.Nightly {
				tagList = append(tagList, agentjob.TagNightly)
			}
		} else {
			fmt.Fprintf(os.Stderr, "jarvis-jobs: add: kanban store: %v — link set, board tag not checked\n", err)
		}
	}
	sp := agentjob.Spec{
		ID:              *id,
		Title:           *title,
		Instructions:    strings.TrimSpace(body),
		DependsOn:       deps,
		Driver:          *driver,
		Source:          *source,
		Class:           *class,
		Priority:        *priority,
		ReasoningEffort: queue.CanonicalEffort(*effort, "coding"),
		KanbanTaskID:    *kanbanTask,
		Tags:            tagList,
	}
	added, err := store.Ingest(sp)
	if err != nil {
		return err
	}
	if !added {
		fmt.Println("exists")
		return nil
	}
	fmt.Println(*id)
	if sp.HasTag(agentjob.TagNightly) {
		fmt.Println("parked: nightly-tagged jobs wait for the NoBrakes auto-enqueue (auto_mode on)")
	}
	return nil
}

// hasTag reports whether the tag list carries tag.
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
