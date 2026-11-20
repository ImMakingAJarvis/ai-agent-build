package reviewsweep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// digestHour is the hour of the day, in Los Angeles, at which the end of
// day digest fires: 18:00 or later.
const digestHour = 18

// lastDigestFile is where the last digest day (YYYY-MM-DD, Los Angeles) is
// stored in the review directory: it is what keeps the sweep from sending a
// second digest on the same day.
const lastDigestFile = "last-digest"

// losAngeles is the calendar the digest runs on.
var losAngeles = loadZone("America/Los_Angeles")

func loadZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// A process that cannot tell Los Angeles days apart must not
		// guess; failing at startup is the honest outcome.
		panic(fmt.Sprintf("reviewsweep: %v", err))
	}
	return loc
}

// Digest builds the end of day digest (Job 10c). At 18:00
// America/Los_Angeles or later, a non-empty rev list yields a string listing
// each PR number and ok true. Before 18:00, or with no revivals, ok is
// false and the string is empty. The caller passes only today's lines
// (reviewstate.RevivalsOn).
func Digest(revs []reviewstate.Revival, now time.Time) (string, bool) {
	if len(revs) == 0 {
		return "", false
	}
	if now.In(losAngeles).Hour() < digestHour {
		return "", false
	}
	nums := make([]int, 0, len(revs))
	seen := make(map[int]bool, len(revs))
	for _, rv := range revs {
		if !seen[rv.PR] {
			seen[rv.PR] = true
			nums = append(nums, rv.PR)
		}
	}
	sort.Ints(nums)
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, strconv.Itoa(n))
	}
	return "review sweeper end of day: PR " + strings.Join(parts, ", "), true
}

// DigestSender delivers the end of day digest: one Discord message and one
// Plane issue. Production wires Discord and Plane behind it; tests use a
// fake.
type DigestSender interface {
	SendDiscord(ctx context.Context, text string) error
	CreateIssue(ctx context.Context, title, body string) (string, error)
}

// SendDigest sends the end of day digest. When Digest says not to send, or a
// digest already went out on this Los Angeles day (last-digest holds it), it
// returns without sending. Otherwise it posts one Discord message, creates
// one Plane issue, and records the day.
func SendDigest(ctx context.Context, dir string, sender DigestSender, revs []reviewstate.Revival, now time.Time) error {
	text, ok := Digest(revs, now)
	if !ok {
		return nil
	}
	day := now.In(losAngeles).Format("2006-01-02")
	path := filepath.Join(dir, lastDigestFile)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == day {
		return nil // already sent on this LA day
	}
	if err := sender.SendDiscord(ctx, text); err != nil {
		return err
	}
	title := fmt.Sprintf("review sweeper: %d revival(s) on %s", len(revs), day)
	body := digestBody(text, revs)
	if _, err := sender.CreateIssue(ctx, title, body); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(day+"\n"), 0o600)
}

// digestBody is the Plane issue body: the digest line plus one line per
// revival, so every PR number from the log appears in it.
func digestBody(text string, revs []reviewstate.Revival) string {
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n\n")
	for _, rv := range revs {
		fmt.Fprintf(&b, "PR %d: %s (enqueued %s)\n", rv.PR, rv.State, rv.Enqueued)
	}
	return strings.TrimRight(b.String(), "\n")
}

// EndOfDay sends today's digest from the review directory's revival log.
func (s *Sweep) EndOfDay(ctx context.Context, sender DigestSender) error {
	now := s.now()
	revs, err := reviewstate.RevivalsOn(s.Dir, now)
	if err != nil {
		return err
	}
	return SendDigest(ctx, s.Dir, sender, revs, now)
}
