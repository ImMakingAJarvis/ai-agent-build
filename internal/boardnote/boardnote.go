// Package boardnote posts model-prefixed Plane comments for board-mapped
// jobs — the visibility half of the event-driven brain's wait/wake loop
// (plan §3.5, EVB-16): a yield and a wake are both cross-model hops, and a
// job that lives on a work board (a Plane display id in its title) says so
// on its card. Every call is best-effort: a missing Plane config, a
// non-mapped job, or a failed post is logged, never an error — the job's
// own wait/wake state is authoritative and must not depend on the board.
package boardnote

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// boardRef is a Plane display id: an uppercase project identifier (2-10
// letters/digits, starting with a letter — "T2I" is a live identifier) and
// a sequence number, word-bounded so "PR EVB-16" or a longer token never
// half-matches.
var boardRef = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-\d+)\b`)

// RefFromTitle returns the first Plane display id in a job title — the
// board mapping. Plane job titles carry it as the first word (plane-bridge
// names them "Plane: run task T2I-3 (name)"); "" means the job is not
// board-mapped and no comment is posted.
func RefFromTitle(title string) string {
	m := boardRef.FindStringSubmatch(title)
	if m == nil {
		return ""
	}
	return m[1]
}

// Model returns the model name for the comment prefix: the round env's
// JARVIS_MODEL when set (agent-run sets it for the running round), else
// the yielding/woken round's driver mapped to its model name.
func Model(j agentjob.Job) string {
	if m := os.Getenv("JARVIS_MODEL"); m != "" {
		return m
	}
	return agentjob.ModelFor(j.RanWith)
}

// postCtx bounds one best-effort board post.
func postCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// commentHTML renders the comment body the same way jarvis-board does: one
// paragraph, HTML-escaped, newlines as <br>, the model name as prefix.
func commentHTML(model, msg string) string {
	escaped := html.EscapeString(model + ": " + strings.TrimSpace(msg))
	return "<p>" + strings.ReplaceAll(escaped, "\n", "<br>") + "</p>"
}

// YieldMessage is the yield comment body (without the model prefix):
// "waiting on <in flight>, will resume to <next step>" — each part the
// first line of the continuation's "In flight" / "Next steps" section, so
// the card says what the job parked on and where it is going (the file
// keeps the full detail).
func YieldMessage(inFlight, next string) string {
	return fmt.Sprintf("waiting on %s, will resume to %s",
		oneLine(inFlight, "the results in flight"), oneLine(next, "its next step"))
}

// WakeMessages are the wake comment bodies (without the model prefix) — one
// line per result when the wait completed, one timeout note when it did
// not. rec.Results is the wake record's corr -> ref map (empty on a
// timeout wake); missing is the number of WaitOn results still absent.
func WakeMessages(rec agentjob.WakeRecord, missing int) []string {
	switch {
	case rec.Reason == agentjob.WakeTimeout:
		return []string{fmt.Sprintf("resumed — wait timed out (%d result(s) missing)", missing)}
	case len(rec.Results) > 0:
		var lines []string
		for _, c := range sortedKeys(rec.Results) {
			lines = append(lines, "resumed, got "+c+": "+refOr(rec.Results[c], "no ref recorded"))
		}
		return lines
	default:
		return []string{"resumed"}
	}
}

// PostYield posts the yield comment on the job's card:
//
//	<model>: waiting on <in flight>, will resume to <next step>
//
// No comment (nil) when the title carries no board ref or Plane is not
// configured; errors are logged and returned for the caller to ignore.
func PostYield(j agentjob.Job, inFlight, next string) error {
	ref := RefFromTitle(j.Title)
	if ref == "" {
		return nil
	}
	return post(ref, Model(j), YieldMessage(inFlight, next))
}

// PostWake posts the wake comments on the job's card:
//
//	<model>: resumed, got c1: <ref>
//	<model>: resumed, got c2: <ref>
//
// or, on a deadline wake,
//
//	<model>: resumed — wait timed out (<n> result(s) missing)
//
// One comment per line, so the card's comment history reads as the job's
// timeline.
func PostWake(j agentjob.Job, rec agentjob.WakeRecord, missing int) error {
	ref := RefFromTitle(j.Title)
	if ref == "" {
		return nil
	}
	var firstErr error
	for _, l := range WakeMessages(rec, missing) {
		if err := post(ref, Model(j), l); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// sortedKeys returns the map's keys sorted — the wake record is small (a
// handful of corr ids), and stable ordering keeps the comment lines
// deterministic.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// oneLine collapses a section body to its first non-blank line, trimmed to
// 120 chars (a continuation section can run paragraphs; the card gets the
// point, the file gets the detail).
func oneLine(s, fallback string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, "-* ")); l != "" {
			if len(l) > 120 {
				l = l[:117] + "..."
			}
			return l
		}
	}
	return fallback
}

func refOr(ref, fallback string) string {
	if strings.TrimSpace(ref) == "" {
		return fallback
	}
	return ref
}

// Post posts a model-prefixed comment on an explicit work item — the
// generic entry point for callers that already hold the board ref (the
// mailbox mirroring in jarvis-mq ask/tell posts
// "<subject> — <body>" on the message's board item; the job's card itself
// comes from its title and uses PostYield/PostWake). Same best-effort
// contract: no Plane config = no comment (nil).
func Post(ref, model, msg string) error { return post(ref, model, msg) }

// post resolves the board ref against the workspace and adds the comment.
// A missing Plane config is not an error: board comments are a
// best-effort overlay on the board-mapped subset of jobs.
func post(ref, model, msg string) error {
	ctx, cancel := postCtx()
	defer cancel()
	c, err := plane.FromEnv()
	if err != nil {
		log.Printf("boardnote: Plane not configured, skipping %s: %v", ref, err)
		return nil
	}
	// A prefixed display id (EVB-17) selects its own project by identifier;
	// the empty projectRef only matters for the (unused) bare-sequence path.
	proj, itemRef, err := plane.ResolveItemRef(ctx, c, "", ref)
	if err != nil {
		log.Printf("boardnote: resolve %s: %v", ref, err)
		return err
	}
	item, err := plane.WorkItemByRef(ctx, c, proj.ID, itemRef)
	if err != nil {
		log.Printf("boardnote: look up %s: %v", ref, err)
		return err
	}
	if _, err := c.AddComment(ctx, proj.ID, item.ID, commentHTML(model, msg)); err != nil {
		log.Printf("boardnote: comment %s: %v", ref, err)
		return err
	}
	log.Printf("boardnote: %s: posted \"%s: %s\"", ref, model, msg)
	return nil
}
