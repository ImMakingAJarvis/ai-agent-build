package reviewsweep

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

func testCfg() reviewcfg.Config {
	return reviewcfg.Config{
		Reviewers: map[string]reviewcfg.Reviewer{
			"sonnet": {Queue: "review.sonnet", Kind: "cloud"},
		},
	}
}

// TestSweep checks the loop (Job 10b): the sweep is not a model, one stuck
// record publishes exactly one review job and logs exactly one revival, and
// a pass that sees the job queued publishes nothing new. It calls Once,
// never the production sleep.
func TestSweep(t *testing.T) {
	// The sweep must never run a model: the command list may not name one.
	for _, c := range commands {
		lc := strings.ToLower(c)
		for _, forbidden := range []string{"claude", "agent-local", "ollama"} {
			if strings.Contains(lc, forbidden) {
				t.Fatalf("commands contains %q; the sweep must never run %q", c, forbidden)
			}
		}
	}

	dir := t.TempDir()
	rec := reviewstate.Record{
		PR: 268, Branch: "phase-x", Head: "abc123",
		ReviewerModel: "sonnet", Token: "sonnet",
	}
	if err := reviewstate.Save(dir, rec); err != nil {
		t.Fatal(err)
	}

	type pub struct {
		msg queue.Message
		q   string
	}
	var published []pub
	s := Sweep{
		Dir: dir,
		Cfg: testCfg(),
		Publish: func(ctx context.Context, m queue.Message, q string) error {
			published = append(published, pub{m, q})
			return nil
		},
		EnqueueAuthor: func(ctx context.Context, r reviewstate.Record) error { return nil },
		Status: func(pr int) Status {
			return Status{Waiting: 5 * time.Minute}
		},
		Now: func() time.Time { return time.Date(2026, 11, 19, 2, 14, 0, 0, time.UTC) },
	}

	if err := s.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d messages, want 1", len(published))
	}
	if got := published[0].q; got != "review.sonnet" {
		t.Errorf("published to queue %q, want review.sonnet", got)
	}
	for key, want := range map[string]string{
		"pr":       "268",
		"branch":   "phase-x",
		"reviewer": "sonnet",
	} {
		if got := published[0].msg.Payload[key]; got != want {
			t.Errorf("payload %s = %q, want %q", key, got, want)
		}
	}

	revs := revivals(t, dir)
	if len(revs) != 1 {
		t.Fatalf("revivals log has %d lines, want 1", len(revs))
	}
	if revs[0].PR != 268 || revs[0].Enqueued != "review.sonnet" {
		t.Errorf("revival line = %+v, want PR 268 enqueued on review.sonnet", revs[0])
	}

	// A second pass with the job already queued publishes nothing new.
	s.Status = func(pr int) Status {
		return Status{JobQueued: true, Waiting: 5 * time.Minute}
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	if len(published) != 1 {
		t.Errorf("after the second pass %d messages published, want 1", len(published))
	}
	if got := len(revivals(t, dir)); got != 1 {
		t.Errorf("after the second pass the revivals log has %d lines, want 1", got)
	}
}

// revivals reads dir/revivals.jsonl and returns its lines as Revivals.
func revivals(t *testing.T, dir string) []reviewstate.Revival {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "revivals.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []reviewstate.Revival
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rv reviewstate.Revival
		if err := json.Unmarshal([]byte(line), &rv); err != nil {
			t.Fatalf("bad revival line %q: %v", line, err)
		}
		out = append(out, rv)
	}
	return out
}
