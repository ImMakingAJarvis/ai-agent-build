package boardnote

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// TestLiveYieldWakeComments is the EVB-16 acceptance-3 demo against the
// real board: it posts a yield comment and the wake comments on a live
// card and verifies they land in the card's comment history. It is
// opt-in — it runs only with BOARDNOTE_LIVE=1 (and Plane configured) so
// an ordinary `go test ./...` never touches the board. The card is named
// by BOARDNOTE_LIVE_REF (default EVB-17, this job's own card), and the
// new comment ids are logged for the PR record.
func TestLiveYieldWakeComments(t *testing.T) {
	if os.Getenv("BOARDNOTE_LIVE") == "" {
		t.Skip("BOARDNOTE_LIVE unset (live board demo is opt-in)")
	}
	ref := os.Getenv("BOARDNOTE_LIVE_REF")
	if ref == "" {
		ref = "EVB-17"
	}
	c, err := plane.FromEnv()
	if err != nil {
		t.Skipf("Plane not configured: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proj, itemRef, err := plane.ResolveItemRef(ctx, c, "", ref)
	if err != nil {
		t.Fatalf("resolve %s: %v", ref, err)
	}
	item, err := plane.WorkItemByRef(ctx, c, proj.ID, itemRef)
	if err != nil {
		t.Fatalf("look up %s: %v", ref, err)
	}
	before, err := c.ListComments(ctx, proj.ID, item.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}

	// The board-mapped job the comments describe: the ref in the title is
	// what maps the job to its card.
	j := agentjob.Job{Spec: agentjob.Spec{Title: "Plane: run task " + ref + " (EVB-16 live demo)"}, State: agentjob.State{RanWith: "local"}}
	if err := PostYield(j, "- demo-corr-1: a result in flight on flow.demo", "- verify the demo result, then finish the job"); err != nil {
		t.Fatalf("PostYield: %v", err)
	}
	rec := agentjob.WakeRecord{Reason: agentjob.WakeAll, Results: map[string]string{"demo-corr-1": "demo-result-ref"}}
	if err := PostWake(j, rec, 0); err != nil {
		t.Fatalf("PostWake: %v", err)
	}

	after, err := c.ListComments(ctx, proj.ID, item.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(after) < len(before)+2 {
		t.Fatalf("expected at least 2 new comments, got %d before, %d after", len(before), len(after))
	}
	// The list order is not chronological on this build, so find the new
	// comments by id diff and log them for the PR record.
	seen := make(map[string]bool, len(before))
	for _, cm := range before {
		seen[cm.ID] = true
	}
	for _, cm := range after {
		if !seen[cm.ID] {
			t.Logf("new comment %s: %s", cm.ID, cm.CommentHTML)
		}
	}
}
