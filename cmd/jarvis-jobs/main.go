// jarvis-jobs is how the agent-run loop works with queued agent jobs.
//
//	jarvis-jobs list [status]              all jobs, or only one status (e.g. failed)
//	jarvis-jobs next                       print "<id>\t<title>"; exit 1 if none ready
//	jarvis-jobs run <id> [-driver local|sonnet] [-round N] [-note TEXT]
//	jarvis-jobs finish <id> <exit-code>    record a round that ran elsewhere (after a reboot)
//	jarvis-jobs skip <id> | requeue <id> | show <id>
//	jarvis-jobs add -id ID -title T [-driver local|sonnet] [-source S] [-priority N]
//	jarvis-jobs message [TEXT]   send the agent a message (stdin when TEXT
//	                              is "-" or absent); injected at its next step
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "jarvis-jobs: "+format+"\n", a...)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: jarvis-jobs list [status]|next|run|finish|skip|requeue|show|pause|add|message")
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
		fmt.Printf("%s — %s\nstatus: %s\n\n%s\n", j.ID, j.Title, j.Status, j.Instructions)
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
		driver := fs.String("driver", "local", "local or sonnet")
		round := fs.String("round", "", "round label")
		note := fs.String("note", "", "typed instructions for this round")
		effort := fs.String("effort", queue.DefaultEffort,
			"reasoning effort: low, medium, high, xhigh, max")
		if len(args) < 1 {
			die("run <id> [flags]")
		}
		fs.Parse(args[1:])
		// Inject effort into the job spec so the runner picks it up.
		if *effort != "" {
			j, err := store.Get(args[0])
			if err == nil {
				j.ReasoningEffort = *effort
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
	case "message":
		if err := cmdMessage(args, os.Stdin); err != nil {
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

func runner(store *agentjob.Store) *agentjob.Runner {
	h, err := queue.NewHistory()
	if err != nil {
		die("history: %v", err)
	}
	r := &agentjob.Runner{Store: store, History: h}
	if conf := os.Getenv("JARVIS_CONF"); conf != "" {
		r.SignalFile = conf + "/pause-signal"
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
	driver := fs.String("driver", "local", "local or sonnet")
	source := fs.String("source", "", "origin (discord, migrate, …)")
	priority := fs.Int("priority", 0, "higher runs first")
	instructions := fs.String("instructions", "", "job body (default: stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *title == "" {
		return fmt.Errorf("add -id ID -title T")
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
		ID:           *id,
		Title:        *title,
		Instructions: strings.TrimSpace(body),
		Driver:       *driver,
		Source:       *source,
		Priority:     *priority,
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
