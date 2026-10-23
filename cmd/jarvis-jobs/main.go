// jarvis-jobs is how the agent-run loop works with queued agent jobs.
//
//	jarvis-jobs list
//	jarvis-jobs next                       print "<id>\t<title>"; exit 1 if none ready
//	jarvis-jobs run <id> [-driver local|sonnet] [-round N] [-note TEXT]
//	jarvis-jobs finish <id> <exit-code>    record a round that ran elsewhere (after a reboot)
//	jarvis-jobs skip <id> | requeue <id> | show <id>
//	jarvis-jobs add -id ID -title T [-driver local|sonnet] [-source S] [-priority N]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planner"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "jarvis-jobs: "+format+"\n", a...)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: jarvis-jobs list|next|run|finish|skip|requeue|show|pause|add|plan")
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
	case "plan":
		if err := cmdPlan(store, args, os.Stdin); err != nil {
			die("%v", err)
		}
	case "list":
		store.Recover()
		jobs, _ := store.List()
		for _, j := range jobs {
			fmt.Printf("%-10s %-40s %s %s\n", j.Status, j.ID, j.Title, j.PRURL)
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
	default:
		die("unknown command %q", cmd)
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
	}
	return r
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

func cmdPlan(store *agentjob.Store, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	source := fs.String("source", "planner", "origin label")
	dryRun := fs.Bool("dry-run", false, "print the plan without enqueueing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Read task description from remaining args or stdin.
	var taskDesc string
	if fs.NArg() > 0 {
		taskDesc = strings.Join(fs.Args(), " ")
	} else {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		taskDesc = string(b)
	}
	taskDesc = strings.TrimSpace(taskDesc)
	if taskDesc == "" {
		return fmt.Errorf("plan: task description required (pass as args or via stdin)")
	}

	// Create the planner.
	p := planner.New(planner.Config{
		Store:  store,
		Source: *source,
	})

	fmt.Fprintf(os.Stderr, "Analyzing task...\n")
	plan, err := p.PlanTask(taskDesc)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nPlan: %s\n", plan.Title)
	if plan.Summary != "" {
		fmt.Fprintf(os.Stderr, "Summary: %s\n", plan.Summary)
	}
	if plan.TotalEst != "" {
		fmt.Fprintf(os.Stderr, "Estimate: %s\n", plan.TotalEst)
	}
	fmt.Fprintf(os.Stderr, "Chunks: %d\n\n", len(plan.Chunks))

	for i, c := range plan.Chunks {
		driver, effort := planner.DifficultyToDriver(c.Difficulty)
		fmt.Fprintf(os.Stderr, "  %d. [%s] %s (difficulty: %d, driver: %s, effort: %s)\n",
			i+1, c.Type, c.Title, c.Difficulty, driver, effort)
	}

	if *dryRun {
		fmt.Fprintln(os.Stderr, "\n(Dry run — not enqueueing)")
		return nil
	}

	fmt.Fprintln(os.Stderr, "\nEnqueuing chunks...")
	jobIDs, err := p.EnqueuePlan(plan)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "\nEnqueued jobs:")
	for _, id := range jobIDs {
		fmt.Println(id)
	}
	return nil
}
