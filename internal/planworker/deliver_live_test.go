package planworker

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// TestLiveB4Deliver is the B4 acceptance against the real board: a toy
// epic in a scratch project ends up with a plan page, sub-items in Plan
// review, blocked-by relations and model: labels, and the epic itself in
// Plan review. The scratch project is deleted afterwards — the live board
// is left exactly as found. Credentials come from the environment (or
// ~/.config/jarvis/agent.env via FromEnv); without them the test skips,
// so `go test ./...` stays green off this machine.
func TestLiveB4Deliver(t *testing.T) {
	c, err := plane.FromEnv()
	if err != nil {
		t.Skipf("plane board unavailable: %v", err)
	}
	defer plane.AcquireLiveLock(t, 6*time.Minute)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	proj, err := c.CreateProject(ctx, fmt.Sprintf("b4_test_%d", time.Now().Unix()), randomIdentifier(t, "B4"))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Logf("scratch project %s (%s)", proj.Name, proj.ID)
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if err := c.DeleteProject(dctx, proj.ID); err != nil {
			t.Errorf("cleanup: delete scratch project %s: %v", proj.Name, err)
		}
	})

	// Fresh scratch projects only get the five default states — create
	// the one the deliverer needs.
	pr, err := c.CreateState(ctx, proj.ID, PlanReviewState, "unstarted", "#FBBF24", 131072)
	if err != nil {
		t.Fatalf("CreateState %q: %v", PlanReviewState, err)
	}

	epic, err := c.CreateWorkItem(ctx, proj.ID, plane.WorkItemCreate{
		Name:            "B4 live: toy epic",
		DescriptionHTML: "<p>Build a toy.</p>",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem epic: %v", err)
	}

	job := &Job{
		Trigger: "plan_epic",
		Board:   Board{Workspace: "jarvis", Project: proj.Identifier, ProjectID: proj.ID},
		Item:    Item{ID: epic.ID, Name: epic.Name, Description: "Build a toy.", SequenceID: epic.SequenceID},
		ModelID: "qwen3.8-27b",
		Effort:  "high",
	}
	res := &Result{
		Plan:  "# Live toy plan\n\nGoal: build a toy. This is the plan text.",
		Tasks: toyTasks(),
		Model: registry.Model{ID: "qwen3.8-27b"},
	}
	d := &Deliverer{Plane: c, Registry: deliverRegistry()}

	if _, err := d.Deliver(ctx, job, res); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// --- the plan page ---
	pages, err := c.ListPages(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if len(pages) != 1 || pages[0].Name != "Plan: "+epic.Name {
		t.Fatalf("pages = %+v, want one page named %q", pages, "Plan: "+epic.Name)
	}
	// The list omits the page body — GetPage is the way to read it back.
	body, err := c.GetPage(ctx, proj.ID, pages[0].ID)
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if !strings.Contains(body.DescriptionHTML, "This is the plan text") {
		t.Errorf("page body %q does not contain the plan", body.DescriptionHTML)
	}

	// --- the sub-items ---
	label, err := c.LabelByName(ctx, proj.ID, placement.ModelLabel("qwen3.8-27b"))
	if err != nil {
		t.Fatalf("LabelByName: %v", err)
	}
	items, err := c.ListWorkItems(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	byTask := map[string]*plane.WorkItem{}
	for i := range items {
		it := &items[i]
		if it.Parent != epic.ID {
			continue
		}
		if tid, ok := markerValue(it.DescriptionHTML, planTaskMarker); ok {
			byTask[tid] = it
		}
	}
	if len(byTask) != 3 {
		t.Fatalf("sub-items = %d, want 3 (t1, t2, t3)", len(byTask))
	}
	for tid, it := range byTask {
		if it.State != pr.ID {
			t.Errorf("task %s: state = %s, want Plan review (%s)", tid, it.State, pr.ID)
		}
		if len(it.Labels) != 1 || it.Labels[0] != label.ID {
			t.Errorf("task %s: labels = %v, want [%s]", tid, it.Labels, label.ID)
		}
	}

	// --- blocked-by relations ---
	r2, err := c.Relations(ctx, proj.ID, byTask["t2"].ID)
	if err != nil {
		t.Fatalf("Relations(t2): %v", err)
	}
	if !fpContainsAll(issueIDs(r2.BlockedBy), byTask["t1"].ID) || len(r2.BlockedBy) != 1 {
		t.Errorf("t2 blocked_by = %+v, want [%s]", r2.BlockedBy, byTask["t1"].ID)
	}
	r3, err := c.Relations(ctx, proj.ID, byTask["t3"].ID)
	if err != nil {
		t.Fatalf("Relations(t3): %v", err)
	}
	if !fpContainsAll(issueIDs(r3.BlockedBy), byTask["t1"].ID, byTask["t2"].ID) || len(r3.BlockedBy) != 2 {
		t.Errorf("t3 blocked_by = %+v, want [%s %s]", r3.BlockedBy, byTask["t1"].ID, byTask["t2"].ID)
	}

	// --- the epic: link, comment, state ---
	got, err := c.GetWorkItem(ctx, proj.ID, epic.ID)
	if err != nil {
		t.Fatalf("GetWorkItem epic: %v", err)
	}
	wantURL := c.PageLink(proj.ID, &pages[0])
	if n := strings.Count(got.DescriptionHTML, planPageMarker); n != 1 {
		t.Errorf("epic description has %d plan-page lines, want 1: %q", n, got.DescriptionHTML)
	}
	if !strings.Contains(got.DescriptionHTML, planPageMarker+wantURL) {
		t.Errorf("epic description does not contain the plan-page link %s: %q", wantURL, got.DescriptionHTML)
	}
	comments, err := c.ListComments(ctx, proj.ID, epic.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	found := false
	for _, cm := range comments {
		if strings.Contains(cm.CommentHTML, "Plan ready") && strings.Contains(cm.CommentHTML, wantURL) {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'Plan ready' summary comment with the page link on the epic: %+v", comments)
	}
	if got.State != pr.ID {
		t.Errorf("epic state = %s, want Plan review (%s)", got.State, pr.ID)
	}

	// --- convergence: a second delivery must not duplicate ---
	if _, err := d.Deliver(ctx, job, res); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	pages2, err := c.ListPages(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListPages (2nd): %v", err)
	}
	if len(pages2) != 1 {
		t.Errorf("pages after re-delivery = %d, want 1", len(pages2))
	}
	items2, err := c.ListWorkItems(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListWorkItems (2nd): %v", err)
	}
	nSubs := 0
	for i := range items2 {
		if items2[i].Parent == epic.ID {
			nSubs++
		}
	}
	if nSubs != 3 {
		t.Errorf("sub-items after re-delivery = %d, want 3", nSubs)
	}
}

// randomIdentifier builds a unique 5-char project identifier (Plane
// identifiers are short uppercase codes, e.g. AAB).
func randomIdentifier(t *testing.T, prefix string) string {
	t.Helper()
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no I/L/O/0/1
	b := make([]byte, 3)
	for i := range b {
		b[i] = alpha[rand.Intn(len(alpha))]
	}
	return prefix + string(b)
}

// issueIDs maps relation refs to their issue ids.
func issueIDs(refs []plane.RelationRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.IssueID)
	}
	return out
}
