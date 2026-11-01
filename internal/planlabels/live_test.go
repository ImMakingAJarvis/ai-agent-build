package planlabels

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// Live test for the A3 acceptance: on the real board, in a scratch
// project, enabling a planner model makes its plan: label appear and
// disabling removes it. Credentials come from the process environment,
// falling back to the daemon env file (~/.config/jarvis/agent.env), via
// plane.FromEnv; without a reachable board the test skips, so
// `go test ./...` stays green off this machine.

// liveClient returns a client for the live board when a reachable board
// is configured (probe: list projects); ok is false otherwise.
func liveClient(t *testing.T) (*plane.Client, bool) {
	t.Helper()
	c, err := plane.FromEnv()
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.ListProjects(ctx); err != nil {
		t.Logf("live board probe failed, skipping: %v", err)
		return nil, false
	}
	return c, true
}

func TestLiveSyncScratchProject(t *testing.T) {
	c, ok := liveClient(t)
	if !ok {
		t.Skip("PLANE_URL/PLANE_API_KEY unavailable or board unreachable (process env or ~/.config/jarvis/agent.env)")
	}
	defer plane.AcquireLiveLock(t, 5*time.Minute)()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	proj, err := c.CreateProject(ctx, fmt.Sprintf("planlabels_test_%d", time.Now().Unix()), randomIdentifier(t))
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

	// A foreign label that Sync must leave alone.
	if _, err := c.CreateLabel(ctx, proj.ID, "epic", "#6366F1"); err != nil {
		t.Fatalf("CreateLabel(epic): %v", err)
	}

	names := func() []string {
		labels, err := c.ListLabels(ctx, proj.ID)
		if err != nil {
			t.Fatalf("ListLabels: %v", err)
		}
		var out []string
		for _, l := range labels {
			out = append(out, l.Name)
		}
		sort.Strings(out)
		return out
	}

	// m-one enabled only → exactly plan:m-one, plus the epic label.
	if err := Sync(ctx, c, []string{proj.ID}, parseDoc(t, registryDoc(true, false)), quietLog); err != nil {
		t.Fatalf("Sync (m-one): %v", err)
	}
	if got := names(); fmt.Sprint(got) != fmt.Sprint([]string{"epic", "plan:m-one"}) {
		t.Fatalf("after enabling m-one: labels = %v, want [epic plan:m-one]", got)
	}

	// Enable m-two → both plan: labels appear.
	if err := Sync(ctx, c, []string{proj.ID}, parseDoc(t, registryDoc(true, true)), quietLog); err != nil {
		t.Fatalf("Sync (m-one+m-two): %v", err)
	}
	if got := names(); fmt.Sprint(got) != fmt.Sprint([]string{"epic", "plan:m-one", "plan:m-two"}) {
		t.Fatalf("after enabling m-two: labels = %v, want [epic plan:m-one plan:m-two]", got)
	}

	// Disable m-one → its label is removed, m-two's stays.
	if err := Sync(ctx, c, []string{proj.ID}, parseDoc(t, registryDoc(false, true)), quietLog); err != nil {
		t.Fatalf("Sync (m-two only): %v", err)
	}
	if got := names(); fmt.Sprint(got) != fmt.Sprint([]string{"epic", "plan:m-two"}) {
		t.Fatalf("after disabling m-one: labels = %v, want [epic plan:m-two]", got)
	}
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
	return "PL" + string(b)
}
