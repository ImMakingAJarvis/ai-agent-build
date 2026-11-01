package plane

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Live test against the real board (P3 acceptance: "tests against a
// scratch project"). It creates a scratch project in the "jarvis"
// workspace, exercises every client operation in it, and deletes the
// project again — the live board is left exactly as found.
//
// Credentials come from the process environment, falling back to the
// daemon env file that scripts/plane-setup.sh writes
// (~/.config/jarvis/agent.env), via FromEnv. Without them the test
// skips, so `go test ./...` stays green off this machine.

// liveClient returns a client for the live board; ok is false when the
// minimum (URL + API key) is unavailable.
func liveClient() (*Client, bool) {
	c, err := FromEnv()
	if err != nil {
		return nil, false
	}
	if c.apiKey == "" {
		return nil, false
	}
	return c, true
}

// randomIdentifier builds a unique 5-char project identifier (Plane
// identifiers are short uppercase codes, e.g. AAB).
func randomIdentifier(t *testing.T) string {
	t.Helper()
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no I/L/O/0/1
	b := make([]byte, 3)
	for i := range b {
		b[i] = alpha[rand.Intn(len(alpha))]
	}
	return "P3" + string(b)
}

func TestLiveScratchProject(t *testing.T) {
	c, ok := liveClient()
	if !ok {
		t.Skip("PLANE_URL/PLANE_API_KEY unavailable (process env or ~/.config/jarvis/agent.env)")
	}
	defer AcquireLiveLock(t, 5*time.Minute)()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := fmt.Sprintf("p3_test_%d", time.Now().Unix())
	proj, err := c.CreateProject(ctx, name, randomIdentifier(t))
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

	// --- work item + sub-item ---
	parent, err := c.CreateWorkItem(ctx, proj.ID, WorkItemCreate{Name: "P3 live: parent"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	child, err := c.CreateWorkItem(ctx, proj.ID, WorkItemCreate{
		Name:   "P3 live: child",
		Parent: parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkItem child: %v", err)
	}
	if child.Parent != parent.ID {
		t.Errorf("child.Parent = %q, want %q", child.Parent, parent.ID)
	}

	// --- set state ---
	done, err := c.StateByName(ctx, proj.ID, "Done")
	if err != nil {
		t.Fatalf("StateByName(Done): %v", err)
	}
	got, err := c.SetState(ctx, proj.ID, parent.ID, done.ID)
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if got.State != done.ID {
		t.Errorf("parent state = %q, want %q", got.State, done.ID)
	}

	// --- labels: add two, remove one (union/replace semantics) ---
	la, err := c.CreateLabel(ctx, proj.ID, "p3-a", "#111111")
	if err != nil {
		t.Fatalf("CreateLabel a: %v", err)
	}
	lb, err := c.CreateLabel(ctx, proj.ID, "p3-b", "#222222")
	if err != nil {
		t.Fatalf("CreateLabel b: %v", err)
	}
	got, err = c.AddLabels(ctx, proj.ID, child.ID, la.ID)
	if err != nil {
		t.Fatalf("AddLabels a: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != la.ID {
		t.Errorf("labels after add(a) = %v, want [%s]", got.Labels, la.ID)
	}
	got, err = c.AddLabels(ctx, proj.ID, child.ID, lb.ID)
	if err != nil {
		t.Fatalf("AddLabels b: %v", err)
	}
	if !hasAll(got.Labels, la.ID, lb.ID) || len(got.Labels) != 2 {
		t.Errorf("labels after add(b) = %v, want both", got.Labels)
	}
	got, err = c.RemoveLabels(ctx, proj.ID, child.ID, la.ID)
	if err != nil {
		t.Fatalf("RemoveLabels: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != lb.ID {
		t.Errorf("labels after remove(a) = %v, want [%s]", got.Labels, lb.ID)
	}

	// --- blocked-by relation ---
	sibling, err := c.CreateWorkItem(ctx, proj.ID, WorkItemCreate{Name: "P3 live: sibling"})
	if err != nil {
		t.Fatalf("CreateWorkItem sibling: %v", err)
	}
	if err := c.AddBlockedBy(ctx, proj.ID, child.ID, sibling.ID); err != nil {
		t.Fatalf("AddBlockedBy: %v", err)
	}
	rels, err := c.Relations(ctx, proj.ID, child.ID)
	if err != nil {
		t.Fatalf("Relations: %v", err)
	}
	found := false
	for _, ref := range rels.BlockedBy {
		if ref.IssueID == sibling.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("blocked_by = %+v, want a ref to %s", rels.BlockedBy, sibling.ID)
	}

	// --- comment ---
	html := "<p>P3 live test: plane client comment</p>"
	cm, err := c.AddComment(ctx, proj.ID, parent.ID, html)
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if cm.CommentHTML != html {
		t.Errorf("comment_html = %q, want %q", cm.CommentHTML, html)
	}

	// --- page (session API) ---
	if c.email == "" || c.password == "" {
		t.Skip("PLANE_BOT_EMAIL/PLANE_BOT_PASSWORD unavailable; page step needs a session")
	}
	pg, err := c.CreatePage(ctx, proj.ID, PageCreate{
		Name:        "P3 live test page",
		ContentHTML: "<p>plane client created this page</p>",
	})
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	if pg.ID == "" || pg.Name != "P3 live test page" {
		t.Errorf("page = %+v", pg)
	}
	pages, err := c.ListPages(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if len(pages) != 1 || pages[0].ID != pg.ID {
		t.Errorf("ListPages = %+v, want the one page %s", pages, pg.ID)
	}
}

func hasAll(have []string, want ...string) bool {
	for _, w := range want {
		ok := false
		for _, h := range have {
			if h == w {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
