package reviewrequest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// testCfg is a review config with the three cloud reviewers; only
// Fable needs the release (the shape of config/review.yaml).
func testCfg() reviewcfg.Config {
	return reviewcfg.Config{
		Reviewers: map[string]reviewcfg.Reviewer{
			"sonnet": {Queue: "review.sonnet", Kind: "cloud", NeedsRelease: false},
			"opus":   {Queue: "review.opus", Kind: "cloud", NeedsRelease: false},
			"fable":  {Queue: "review.fable", Kind: "cloud", NeedsRelease: true},
		},
	}
}

type published struct {
	q string
	m queue.Message
}

// newService wires a Service against a temp record dir with a fake
// publisher (a function field, no broker) recording what was sent.
func newService(t *testing.T) (*Service, *[]published) {
	t.Helper()
	pubs := new([]published)
	s := &Service{
		Dir: t.TempDir(),
		Cfg: testCfg(),
		Publish: func(ctx context.Context, m queue.Message, q string) error {
			*pubs = append(*pubs, published{q: q, m: m})
			return nil
		},
	}
	return s, pubs
}

func TestRequestUnknownReviewer(t *testing.T) {
	s, pubs := newService(t)
	_, _, err := s.RequestReview(context.Background(), 42, "phase-x", "gpt", "REV-1", "abc123")
	if err == nil {
		t.Fatal("unknown reviewer: err = nil, want the reviewroute error")
	}
	if !strings.Contains(err.Error(), "gpt") {
		t.Errorf("error %q does not name the reviewer", err)
	}
	if len(*pubs) != 0 {
		t.Errorf("published %d messages, want 0", len(*pubs))
	}
	if _, err := reviewstate.Load(s.Dir, 42); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record for PR 42 exists, want none (err = %v)", err)
	}
}

func TestRequestPublishes(t *testing.T) {
	s, pubs := newService(t)
	jobID, held, err := s.RequestReview(context.Background(), 42, "phase-x", "sonnet", "REV-1", "abc123")
	if err != nil {
		t.Fatalf("RequestReview: %v", err)
	}
	if held {
		t.Error("held = true, want false for a cloud reviewer without needs_release")
	}
	if len(*pubs) != 1 {
		t.Fatalf("published %d messages, want exactly 1", len(*pubs))
	}
	p := (*pubs)[0]
	if p.q != "review.sonnet" {
		t.Errorf("published to queue %q, want review.sonnet", p.q)
	}
	// The message body has the PR number and the branch.
	if got := p.m.Payload["pr"]; got != "42" {
		t.Errorf("payload pr = %q, want %q", got, "42")
	}
	if got := p.m.Payload["branch"]; got != "phase-x" {
		t.Errorf("payload branch = %q, want %q", got, "phase-x")
	}
	if jobID == "" || jobID != p.m.ID {
		t.Errorf("jobID = %q, want the published message id %q", jobID, p.m.ID)
	}

	rec, err := reviewstate.Load(s.Dir, 42)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.ReviewerModel != "sonnet" || rec.Token != "sonnet" {
		t.Errorf("record reviewer/token = %q/%q, want sonnet/sonnet", rec.ReviewerModel, rec.Token)
	}
	if rec.Head != "abc123" || rec.Branch != "phase-x" || rec.PlaneItem != "REV-1" {
		t.Errorf("record head/branch/plane = %q/%q/%q, want abc123/phase-x/REV-1", rec.Head, rec.Branch, rec.PlaneItem)
	}
	if rec.Held {
		t.Error("record Held = true, want false")
	}
	// A new record leaves the stamps empty until the author job sets them.
	if rec.AuthorStamp != "" || rec.ReviewerStamp != "" {
		t.Errorf("stamps = %q/%q on a new record, want empty", rec.AuthorStamp, rec.ReviewerStamp)
	}
}

func TestRequestDedup(t *testing.T) {
	s, pubs := newService(t)
	if _, _, err := s.RequestReview(context.Background(), 42, "phase-x", "sonnet", "REV-1", "abc123"); err != nil {
		t.Fatalf("first RequestReview: %v", err)
	}
	// Same PR and reviewer, head unchanged: no second message.
	jobID, held, err := s.RequestReview(context.Background(), 42, "phase-x", "sonnet", "REV-1", "abc123")
	if err != nil {
		t.Fatalf("second RequestReview: %v", err)
	}
	if held {
		t.Error("held = true on the deduped call, want false")
	}
	if jobID != "" {
		t.Errorf("jobID = %q on the deduped call, want empty (nothing new)", jobID)
	}
	if len(*pubs) != 1 {
		t.Fatalf("published %d messages after the same request twice, want 1", len(*pubs))
	}
	// A new head re-publishes: the review is for a different commit.
	if _, _, err := s.RequestReview(context.Background(), 42, "phase-x", "sonnet", "REV-1", "def456"); err != nil {
		t.Fatalf("RequestReview at new head: %v", err)
	}
	if len(*pubs) != 2 {
		t.Errorf("published %d messages after a new head, want 2", len(*pubs))
	}
	rec, err := reviewstate.Load(s.Dir, 42)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Head != "def456" {
		t.Errorf("record head = %q, want def456", rec.Head)
	}
}

func TestFableHeld(t *testing.T) {
	s, pubs := newService(t)
	jobID, held, err := s.RequestReview(context.Background(), 77, "phase-y", "fable", "REV-2", "abc123")
	if err != nil {
		t.Fatalf("RequestReview(fable): %v", err)
	}
	if !held {
		t.Error("held = false, want true for Fable")
	}
	if jobID != "review-held-77-fable" {
		t.Errorf("jobID = %q, want review-held-77-fable", jobID)
	}
	if len(*pubs) != 0 {
		t.Errorf("published %d messages, want 0 while held", len(*pubs))
	}
	rec, err := reviewstate.Load(s.Dir, 77)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rec.Held {
		t.Error("record Held = false, want true")
	}
	if rec.ReviewerModel != "fable" || rec.Token != "fable" {
		t.Errorf("record reviewer/token = %q/%q, want fable/fable", rec.ReviewerModel, rec.Token)
	}
	// A second request for the same held PR publishes nothing and keeps
	// the stable job id.
	jobID2, held2, err := s.RequestReview(context.Background(), 77, "phase-y", "fable", "REV-2", "abc123")
	if err != nil {
		t.Fatalf("second RequestReview(fable): %v", err)
	}
	if !held2 || jobID2 != "review-held-77-fable" {
		t.Errorf("second call = (%q, held %v), want the stable held id", jobID2, held2)
	}
	if len(*pubs) != 0 {
		t.Errorf("published %d messages after the second held request, want 0", len(*pubs))
	}
}

func TestFableRelease(t *testing.T) {
	s, pubs := newService(t)
	if _, held, err := s.RequestReview(context.Background(), 77, "phase-y", "fable", "REV-2", "abc123"); err != nil || !held {
		t.Fatalf("RequestReview(fable) = held %v, err %v", held, err)
	}
	if err := s.Release(context.Background(), 77); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(*pubs) != 1 {
		t.Fatalf("published %d messages after Release, want 1", len(*pubs))
	}
	p := (*pubs)[0]
	if p.q != "review.fable" {
		t.Errorf("released to queue %q, want review.fable", p.q)
	}
	if got := p.m.Payload["pr"]; got != "77" {
		t.Errorf("payload pr = %q, want %q", got, "77")
	}
	if got := p.m.Payload["branch"]; got != "phase-y" {
		t.Errorf("payload branch = %q, want %q", got, "phase-y")
	}
	rec, err := reviewstate.Load(s.Dir, 77)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Held {
		t.Error("record Held = true after Release, want false")
	}
	// A second Release publishes nothing.
	if err := s.Release(context.Background(), 77); err == nil {
		t.Fatal("second Release: err = nil, want an error that contains held")
	} else if !strings.Contains(err.Error(), "held") {
		t.Errorf("second Release error %q does not contain %q", err.Error(), "held")
	}
	if len(*pubs) != 1 {
		t.Errorf("published %d messages after the second Release, want still 1", len(*pubs))
	}
}

func TestReleaseNotHeld(t *testing.T) {
	s, pubs := newService(t)
	// A normal (unheld) request: releasing it is an error that
	// contains "held" and publishes nothing.
	if _, _, err := s.RequestReview(context.Background(), 42, "phase-x", "sonnet", "REV-1", "abc123"); err != nil {
		t.Fatalf("RequestReview(sonnet): %v", err)
	}
	err := s.Release(context.Background(), 42)
	if err == nil {
		t.Fatal("Release on an unheld record: err = nil, want an error that contains held")
	}
	if !strings.Contains(err.Error(), "held") {
		t.Errorf("error %q does not contain %q", err.Error(), "held")
	}
	if len(*pubs) != 1 {
		t.Errorf("published %d messages, want the original 1 only", len(*pubs))
	}
	// And releasing a PR with no record at all is an error.
	if err := s.Release(context.Background(), 99); err == nil {
		t.Error("Release on a missing record: err = nil, want an error")
	}
}
