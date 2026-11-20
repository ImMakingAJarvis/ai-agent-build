package reviewsweep

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// fakeSender is the test DigestSender: it counts its calls and records what
// it was handed.
type fakeSender struct {
	discord []string
	titles  []string
	bodies  []string
}

func (f *fakeSender) SendDiscord(ctx context.Context, text string) error {
	f.discord = append(f.discord, text)
	return nil
}

func (f *fakeSender) CreateIssue(ctx context.Context, title, body string) (string, error) {
	f.titles = append(f.titles, title)
	f.bodies = append(f.bodies, body)
	return "REV-99", nil
}

func TestDigest(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time {
		return time.Date(2026, 11, 19, h, m, 0, 0, la)
	}
	revs := []reviewstate.Revival{
		{At: at(18, 14), PR: 268, State: "waiting", Enqueued: "review.sonnet"},
		{At: at(19, 0), PR: 271, State: "waiting", Enqueued: "review.opus"},
	}

	t.Run("18:00 Los Angeles with revivals sends", func(t *testing.T) {
		s, ok := Digest(revs, at(18, 0))
		if !ok {
			t.Fatalf("Digest at 18:00 is not ok")
		}
		for _, n := range []int{268, 271} {
			if !strings.Contains(s, strconv.Itoa(n)) {
				t.Errorf("digest %q does not list PR %d", s, n)
			}
		}
	})

	t.Run("17:59 Los Angeles does not send even with revivals", func(t *testing.T) {
		s, ok := Digest(revs, at(17, 59))
		if ok || s != "" {
			t.Errorf("Digest at 17:59 = (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("no revivals does not send", func(t *testing.T) {
		s, ok := Digest(nil, at(18, 0))
		if ok || s != "" {
			t.Errorf("Digest with no revs = (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("ok sends one Discord message and one Plane issue", func(t *testing.T) {
		dir := t.TempDir()
		f := &fakeSender{}
		now := at(18, 30)
		if err := SendDigest(context.Background(), dir, f, revs, now); err != nil {
			t.Fatal(err)
		}
		if len(f.discord) != 1 {
			t.Errorf("discord calls = %d, want 1", len(f.discord))
		}
		if len(f.titles) != 1 {
			t.Fatalf("issue calls = %d, want 1", len(f.titles))
		}
		if !strings.Contains(f.titles[0], "review sweeper") {
			t.Errorf("issue title %q does not contain review sweeper", f.titles[0])
		}
		for _, n := range []int{268, 271} {
			if !strings.Contains(f.bodies[0], strconv.Itoa(n)) {
				t.Errorf("issue body does not contain PR %d: %q", n, f.bodies[0])
			}
		}
		b, err := os.ReadFile(filepath.Join(dir, lastDigestFile))
		if err != nil {
			t.Fatalf("last-digest not written: %v", err)
		}
		if got := strings.TrimSpace(string(b)); got != "2026-11-19" {
			t.Errorf("last-digest = %q, want 2026-11-19", got)
		}
	})

	t.Run("not ok sends nothing", func(t *testing.T) {
		dir := t.TempDir()
		f := &fakeSender{}
		if err := SendDigest(context.Background(), dir, f, revs, at(17, 59)); err != nil {
			t.Fatal(err)
		}
		if len(f.discord) != 0 || len(f.titles) != 0 {
			t.Errorf("sender was called: discord %d, issues %d, want 0 of each",
				len(f.discord), len(f.titles))
		}
	})

	t.Run("no second digest the same Los Angeles day", func(t *testing.T) {
		dir := t.TempDir()
		f := &fakeSender{}
		if err := SendDigest(context.Background(), dir, f, revs, at(18, 0)); err != nil {
			t.Fatal(err)
		}
		// A later pass the same day (23:30) sends nothing.
		if err := SendDigest(context.Background(), dir, f, revs, at(23, 30)); err != nil {
			t.Fatal(err)
		}
		if len(f.discord) != 1 || len(f.titles) != 1 {
			t.Errorf("second digest went out: discord %d, issues %d, want 1 of each",
				len(f.discord), len(f.titles))
		}
	})

	t.Run("the next Los Angeles day sends again", func(t *testing.T) {
		dir := t.TempDir()
		f := &fakeSender{}
		if err := SendDigest(context.Background(), dir, f, revs, at(18, 0)); err != nil {
			t.Fatal(err)
		}
		next := time.Date(2026, 11, 20, 18, 0, 0, 0, la)
		if err := SendDigest(context.Background(), dir, f, revs, next); err != nil {
			t.Fatal(err)
		}
		if len(f.discord) != 2 || len(f.titles) != 2 {
			t.Errorf("digests after the next day: discord %d, issues %d, want 2 of each",
				len(f.discord), len(f.titles))
		}
	})

	t.Run("EndOfDay reads the revival log and sends once", func(t *testing.T) {
		dir := t.TempDir()
		// 02:14 UTC on the 20th is 18:14 on the 19th in Los Angeles in
		// November (standard time, UTC-8).
		utc := time.Date(2026, 11, 20, 2, 14, 0, 0, time.UTC)
		if err := reviewstate.AppendRevival(dir, reviewstate.Revival{
			At: utc, PR: 268, State: "waiting", Enqueued: "review.sonnet",
		}); err != nil {
			t.Fatal(err)
		}
		f := &fakeSender{}
		s := Sweep{Dir: dir, Now: func() time.Time { return at(19, 0) }}
		if err := s.EndOfDay(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		if len(f.discord) != 1 || len(f.titles) != 1 {
			t.Fatalf("EndOfDay sent discord %d, issues %d, want 1 of each",
				len(f.discord), len(f.titles))
		}
		if !strings.Contains(f.bodies[0], "268") {
			t.Errorf("issue body does not contain PR 268: %q", f.bodies[0])
		}
	})
}
