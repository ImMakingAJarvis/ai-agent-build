package main

import (
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// TestLiveStateCommand exercises the `state` and `done` subcommands
// end-to-end against a scratch project (job chat-1793781308-8f20: when the
// worker is done with a task, it updates the Plane item). Like the live
// tests in internal/plane, it skips without board credentials and leaves
// the board exactly as found.
func TestLiveStateCommand(t *testing.T) {
	c, err := plane.FromEnv()
	if err != nil {
		t.Skipf("PLANE env unavailable: %v", err)
	}
	defer plane.AcquireLiveLock(t, 5*time.Minute)()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := fmt.Sprintf("p6b_test_%d", time.Now().Unix())
	proj, err := c.CreateProject(ctx, name, randomIdentifier(t))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Logf("scratch project %s (%s)", proj.Name, proj.Identifier)
	defer c.DeleteProject(ctx, proj.ID)

	wi, err := c.CreateWorkItem(ctx, proj.ID, plane.WorkItemCreate{Name: "state command test"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	ref := fmt.Sprintf("%s-%d", proj.Identifier, wi.SequenceID)

	bin := filepath.Join(t.TempDir(), "jarvis-board")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	stateID := func(name string) string {
		t.Helper()
		st, err := c.StateByName(ctx, proj.ID, name)
		if err != nil {
			t.Fatalf("state %q: %v", name, err)
		}
		return st.ID
	}
	assertState := func(want string) {
		t.Helper()
		got, err := plane.WorkItemByRef(ctx, c, proj.ID, ref)
		if err != nil {
			t.Fatalf("WorkItemByRef: %v", err)
		}
		if got.State != stateID(want) {
			t.Fatalf("state = %q, want %q", got.State, want)
		}
	}

	// "state" moves the item; the state name matches case-insensitively.
	if out, err := run("state", ref, "in progress"); err != nil {
		t.Fatalf("state: %v\n%s", err, out)
	}
	assertState("In Progress")

	// "done" is the completion shortcut: it moves to Done and posts the
	// trailing message as a model-prefixed comment.
	out, err := run("done", ref, "done: live test, no PR")
	if err != nil {
		t.Fatalf("done: %v\n%s", err, out)
	}
	if !strings.Contains(out, ref) || !strings.Contains(out, "Done") {
		t.Fatalf("done output %q does not confirm the move", out)
	}
	assertState("Done")
	cms, err := c.ListComments(ctx, proj.ID, wi.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	found := false
	for i := range cms {
		if strings.Contains(cms[i].CommentHTML, "done: live test, no PR") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no comment posted by `done`; got %d comments", len(cms))
	}

	// An unknown state fails loudly instead of moving the item.
	if _, err := run("state", ref, "No Such State"); err == nil {
		t.Fatal("state with unknown name succeeded, want error")
	}
	assertState("Done")
}

// randomIdentifier builds a unique 5-char project identifier (Plane
// identifiers are short uppercase codes, e.g. AAB).
func randomIdentifier(t *testing.T) string {
	t.Helper()
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no I/L/O/0/1
	b := make([]byte, 2)
	for i := range b {
		b[i] = alpha[rand.Intn(len(alpha))]
	}
	return "P6B" + string(b)
}
