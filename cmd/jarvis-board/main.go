// jarvis-board is the CLI for the agent's identity on the Plane board
// (P6 of docs/work-board/PLAN.md): every item the agent touches gets a
// model-prefixed comment, and status messages carry the board link.
//
//	jarvis-board link <item>                print the browser URL for an item
//	jarvis-board item <item>                one line: AAB-4 [state] title link
//	jarvis-board comment <item> [msg...]    post "<model>: <msg>" (stdin if no msg)
//
// <item> is a work-item reference: a UUID or a display id like "AAB-43"
// (a bare sequence "43" also works).
//
// Flags:
//
//	--project <ref>   project to look items up in (default $PLANE_PROJECT, "AAB")
//	--model <name>    model name for the comment prefix (default $JARVIS_MODEL,
//	                  "jarvis-brain")
//
// Config comes from the PLANE_* environment, falling back to the daemon env
// file (~/.config/jarvis/agent.env) via plane.FromEnv.
package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

const usage = `usage:
  jarvis-board link <item>
  jarvis-board item <item>
  jarvis-board comment <item> [message...]

flags:
  --project <ref>   project ref (default $PLANE_PROJECT or "AAB")
  --model <name>    model name for the comment prefix
                    (default $JARVIS_MODEL or "jarvis-brain")

<item> is a UUID or a display id like AAB-43`

// opts is the resolved command-line configuration.
type opts struct {
	project string
	model   string
}

func main() {
	if len(os.Args) < 3 {
		fail(usage)
	}
	o := &opts{}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	fs.StringVar(&o.project, "project", "", "project ref")
	fs.StringVar(&o.model, "model", "", "model name for the comment prefix")
	fs.Parse(os.Args[2:])
	args := fs.Args()
	if len(args) < 1 {
		fail(usage)
	}

	o.project = firstNonEmpty(o.project, os.Getenv("PLANE_PROJECT"), "AAB")
	o.model = firstNonEmpty(o.model, os.Getenv("JARVIS_MODEL"), "jarvis-brain")

	c, err := plane.FromEnv()
	if err != nil {
		fail(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	p, err := plane.ProjectByRef(ctx, c, o.project)
	if err != nil {
		fail(err.Error())
	}
	wi, err := plane.WorkItemByRef(ctx, c, p.ID, args[0])
	if err != nil {
		fail(err.Error())
	}
	link := c.ItemLink(p, wi)
	ref := fmt.Sprintf("%s-%d", p.Identifier, wi.SequenceID)

	switch args[0] {
	case "link":
		fmt.Println(link)

	case "item":
		state, err := stateName(ctx, c, p.ID, wi.State)
		if err != nil {
			fail(err.Error())
		}
		fmt.Printf("%s [%s] %s %s\n", ref, state, wi.Name, link)

	case "comment":
		msg := message(args[1:])
		if msg == "" {
			fail("empty comment")
		}
		cm, err := c.AddComment(ctx, p.ID, wi.ID, commentHTML(o.model, msg))
		if err != nil {
			fail(err.Error())
		}
		fmt.Printf("%s commented: %s\n", cm.ID, link)

	default:
		fail(usage)
	}
}

// commentHTML wraps a model-prefixed message in the comment_html
// convention used on the board: one <p>, text escaped. Newlines become
// <br> so stdin messages keep their shape.
func commentHTML(model, msg string) string {
	escaped := html.EscapeString(model + ": " + strings.TrimSpace(msg))
	return "<p>" + strings.ReplaceAll(escaped, "\n", "<br>") + "</p>"
}

// stateName resolves a state id to its name ("" when unknown).
func stateName(ctx context.Context, c *plane.Client, projectID, stateID string) (string, error) {
	if stateID == "" {
		return "", nil
	}
	states, err := c.ListStates(ctx, projectID)
	if err != nil {
		return "", err
	}
	for i := range states {
		if states[i].ID == stateID {
			return states[i].Name, nil
		}
	}
	return "unknown", nil
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// message joins args, or reads stdin when there are none.
func message(args []string) string {
	if len(args) > 0 {
		return strings.Join(args, " ")
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err.Error())
	}
	return strings.TrimSpace(string(b))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "jarvis-board:", msg)
	os.Exit(1)
}
