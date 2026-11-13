// jarvis-kanban is the worker's CLI for the kanban board's comment system:
// read a task with its epic, subtasks and discussion in one call, and post
// questions or notes as comments.
//
//	jarvis-kanban show <task>                 print task + epic + subtasks + comments
//	jarvis-kanban comment <task> [text...]    add a comment (author: agent; stdin if no text)
//	jarvis-kanban comments <task>             print just the comments, oldest first
//
// <task> is a full task UUID or an 8-char (or longer) prefix of one, as the
// board entry is referenced in job titles.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kanban"
)

const usage = `usage:
  jarvis-kanban show <task>                 task + epic + subtasks + comments
  jarvis-kanban comment <task> [text...]    add a comment (author: agent; stdin if no text)
  jarvis-kanban comments <task>             just the comments, oldest first

<task> is a task UUID or an 8-char prefix of one.`

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "jarvis-kanban:", msg)
	os.Exit(1)
}

// resolveTask loads the task for a full UUID or a unique file prefix, the
// same way the board is referenced in job titles.
func resolveTask(s *kanban.Store, ref string) (kanban.Task, error) {
	if t, err := s.GetTask(ref); err == nil {
		return t, nil
	}
	matches, _ := filepath.Glob(filepath.Join(s.Root(), "tasks", ref+"*.json"))
	switch len(matches) {
	case 0:
		return kanban.Task{}, fmt.Errorf("no task matching %q", ref)
	case 1:
		id := strings.TrimSuffix(filepath.Base(matches[0]), ".json")
		return s.GetTask(id)
	default:
		return kanban.Task{}, fmt.Errorf("ambiguous task ref %q: %s", ref, strings.Join(matches, ", "))
	}
}

// printJSON writes v as indented JSON on its own line.
func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err.Error())
	}
	fmt.Println(string(b))
}

func main() {
	if len(os.Args) < 3 {
		fail(usage)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Parse(os.Args[2:])
	args := fs.Args()

	s, err := kanban.Open(kanban.DataDir())
	if err != nil {
		fail(err.Error())
	}

	switch cmd {
	case "show":
		if len(args) != 1 {
			fail(usage)
		}
		t, err := resolveTask(s, args[0])
		if err != nil {
			fail(err.Error())
		}
		printJSON(t)
		if ep, err := s.GetEpic(t.EpicID); err == nil {
			fmt.Println()
			fmt.Printf("== epic %s ==\n", ep.ID)
			printJSON(ep)
		} else {
			fmt.Println()
			fmt.Printf("(no epic for %s)\n", t.EpicID)
		}
		sts, err := s.ListSubTasks(t.ID)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println()
		fmt.Printf("== subtasks (%d) ==\n", len(sts))
		printJSON(sts)
		cs, err := s.ListComments(t.ID)
		if err != nil {
			fail(err.Error())
		}
		fmt.Println()
		fmt.Printf("== comments (%d) ==\n", len(cs))
		printJSON(cs)

	case "comment", "comments":
		if len(args) < 1 {
			fail(usage)
		}
		t, err := resolveTask(s, args[0])
		if err != nil {
			fail(err.Error())
		}
		if cmd == "comments" {
			cs, err := s.ListComments(t.ID)
			if err != nil {
				fail(err.Error())
			}
			printJSON(cs)
			return
		}
		var body string
		if len(args) > 1 {
			body = strings.TrimSpace(strings.Join(args[1:], " "))
		} else {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				fail(err.Error())
			}
			body = strings.TrimSpace(string(b))
		}
		if body == "" {
			fail("empty comment")
		}
		c, err := s.AddComment(t.ID, "agent", body)
		if err != nil {
			fail(err.Error())
		}
		printJSON(c)

	default:
		fail(usage)
	}
}
