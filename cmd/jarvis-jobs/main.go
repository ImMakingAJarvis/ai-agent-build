// jarvis-jobs is how the agent-run loop works with queued agent jobs.
//
//	jarvis-jobs list [status]              all jobs, or only one status (e.g. failed)
//	jarvis-jobs next                       print "<id>\t<title>"; exit 1 if none ready
//	jarvis-jobs run <id> [-driver local|igpu|sonnet] [-round N] [-note TEXT]
//	jarvis-jobs finish <id> <exit-code>    record a round that ran elsewhere (after a reboot)
//	jarvis-jobs skip <id> | requeue <id> | show <id>
//	jarvis-jobs add -id ID -title T [-driver local|igpu|sonnet] [-source S]
//	                   [-class fix|build|background] [-priority N] [-depends a,b]
//	jarvis-jobs order [a,b,c]    show or set the runtime band priority order
//	                              (a permutation of fix, build, background)
//	jarvis-jobs message [TEXT]   send the agent a message (stdin when TEXT
//	                              is "-" or absent); injected at its next step
//	jarvis-jobs restart [--now]  restart the agent-run service (deferred
//	                              until the current round ends unless --now)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/power"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "jarvis-jobs: "+format+"\n", a...)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: jarvis-jobs list [status]|next|run|finish|restart|skip|requeue|show|pause|add|order|message")
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
		j, err := store.Next()
		if err != nil {
			die("%v", err)
		}
		if j == nil {
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\n", j.ID, j.Title)
	case "show":
		if len(args) != 1 {
			die("show <id>")
		}
		j, err := store.Get(args[0])
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("%s — %s\nstatus: %s\nclass: %s\neffort: %s\n\n%s\n", j.ID, j.Title, j.Status,
			agentjob.EffectiveClass(j.Class, j.Source), queue.CanonicalEffort(j.ReasoningEffort, "coding"), j.Instructions)
	case "pause":
		if len(args) != 1 {
			die("pause <id>")
		}
		if err := store.Pause(args[0]); err != nil {
			die("%v", err)
		}
	case "skip", "requeue":
		if len(args) != 1 {
			die("%s <id>", cmd)
		}
		st := agentjob.State{Status: agentjob.Ready}
		if cmd == "skip" {
			st = agentjob.State{Status: agentjob.Skipped}
		}
		if err := store.SetStatus(args[0], st); err != nil {
			die("%v", err)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		driver := fs.String("driver", "local", "local, igpu or sonnet")
		round := fs.String("round", "", "round label")
		note := fs.String("note", "", "typed instructions for this round")
		effort := fs.String("effort", "",
			"reasoning effort for this round: off, low, medium, high, xhigh, max (default: job spec, else medium)")
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
	if conf := os.Getenv("JARVIS_CONF"); conf != "" {
		r.SignalFile = conf + "/pause-signal"
		// !restart DM (jarvis-chat) writes this; the signal watcher kills
		// the round and re-queues the job for a fresh run.
		r.RestartSignalFile = conf + "/restart-signal"
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
	for _, st := range []string{agentjob.Ready, agentjob.Claimed, agentjob.Running,
		agentjob.Paused, agentjob.Done, agentjob.Failed, agentjob.Skipped} {
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
	driver := fs.String("driver", "local", "local, igpu or sonnet")
	source := fs.String("source", "", "origin (discord, migrate, …)")
	class := fs.String("class", agentjob.ClassBuild, "fix, build or background (fix runs before build, build before background)")
	priority := fs.Int("priority", 0, "higher runs first within the class")
	instructions := fs.String("instructions", "", "job body (default: stdin)")
	effort := fs.String("effort", "", "reasoning effort: off, low, medium, high, xhigh, max (default medium)")
	depends := fs.String("depends", "", "comma-separated job ids this job waits on (they must be done first)")
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
	added, err := store.Ingest(agentjob.Spec{
		ID:              *id,
		Title:           *title,
		Instructions:    strings.TrimSpace(body),
		DependsOn:       deps,
		Driver:          *driver,
		Source:          *source,
		Class:           *class,
		Priority:        *priority,
		ReasoningEffort: queue.CanonicalEffort(*effort, "coding"),
	})
	if err != nil {
		return err
	}
	if !added {
		fmt.Println("exists")
		return nil
	}
	fmt.Println(*id)
	return nil
}
