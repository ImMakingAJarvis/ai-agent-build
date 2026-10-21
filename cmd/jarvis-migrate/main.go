// jarvis-migrate publishes the unchecked items of TASKS.md as agent jobs on
// the queue: J0's successors (D0, D1-D8, Q1), then the benchmarks, then the
// rest in file order. It is idempotent: ids already published (recorded in
// <data>/migrated.txt) or already in the job store are not published again.
//
//	jarvis-migrate [-file TASKS.md] [-dry-run] [-skip "Phase 1"]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func main() {
	file := flag.String("file", "TASKS.md", "task list to migrate")
	dry := flag.Bool("dry-run", false, "print the plan, publish nothing")
	skip := flag.String("skip", "Phase 1", "comma-separated H2 prefixes to leave out (already installed and verified)")
	flag.Parse()

	md, err := os.ReadFile(*file)
	if err != nil {
		log.Fatal(err)
	}
	specs := agentjob.Plan(string(md), time.Now().UTC(), strings.Split(*skip, ","))

	dir := agentjob.DataDir()
	store, err := agentjob.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	ledger := filepath.Join(dir, "migrated.txt")
	done := map[string]bool{}
	if f, err := os.Open(ledger); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			done[strings.TrimSpace(sc.Text())] = true
		}
		f.Close()
	}

	var conn *queue.Conn
	if !*dry {
		if conn, err = queue.NewConn(); err != nil {
			log.Fatalf("connect: %v", err)
		}
		defer conn.Close()
	}
	lf, err := os.OpenFile(ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer lf.Close()

	added := 0
	for _, sp := range specs {
		if done[sp.ID] {
			continue
		}
		if _, err := store.Get(sp.ID); err == nil {
			continue
		}
		fmt.Printf("%-60s p=%d deps=%v\n", sp.ID, sp.Priority, sp.DependsOn)
		added++
		if *dry {
			continue
		}
		msg := queue.Message{
			ID: sp.ID, Type: queue.BackgroundQueue, Priority: sp.Priority, CreatedAt: sp.CreatedAt,
			Source: "migrate", Kind: queue.KindAgent, Title: sp.Title, Instructions: sp.Instructions,
			Branch: sp.Branch, DependsOn: sp.DependsOn, Driver: sp.Driver,
			Payload: map[string]string{},
		}
		if err := conn.Publish(msg, "background.agent"); err != nil {
			log.Fatalf("publish %s: %v", sp.ID, err)
		}
		fmt.Fprintln(lf, sp.ID)
	}
	fmt.Printf("%d job(s) added, %d total in plan\n", added, len(specs))
}
