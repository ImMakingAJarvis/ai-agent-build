package reviewsweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewroute"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// commands is every command this process runs. It is empty on purpose:
// the five-minute check is not a model — it must never run claude,
// agent-local or ollama, and TestSweep fails if one of those strings
// ever lands on this list.
var commands = []string{}

// Status is what the sweep can see about a PR's jobs outside the review
// record: whether the expected next job is already queued or running, how
// long the record has waited, and whether Nathan was already asked.
type Status struct {
	JobQueued    bool
	JobRunning   bool
	Waiting      time.Duration
	AlreadyAsked bool
}

// Sweep is one pass of the five-minute check (Job 10b). It walks the
// review records, runs Next on each, and does what the action says:
// enqueue-reviewer publishes the review job through the same publisher
// as Job 4 and logs the revival; enqueue-author adds the author job and
// logs the revival; ask-nathan is logged.
type Sweep struct {
	// Dir is where the reviewstate records live (reviewstate.DefaultDir).
	Dir string
	// Cfg is the loaded review config (config/review.yaml).
	Cfg reviewcfg.Config
	// Publish delivers one message to a review queue by name — the same
	// publisher as reviewrequest.Service in Job 4.
	Publish func(ctx context.Context, m queue.Message, q string) error
	// EnqueueAuthor adds the author response job (Job 7c) to the agent job
	// store.
	EnqueueAuthor func(ctx context.Context, r reviewstate.Record) error
	// Status reports the queue/running/waiting state for a PR.
	Status func(pr int) Status
	// Now returns the current time (tests pin it).
	Now func() time.Time
}

// Once runs one pass: for each record in Dir it decides the next action and
// acts on it. A pass never sleeps; the caller (cmd/review-sweep) spaces the
// passes five minutes apart.
func (s *Sweep) Once(ctx context.Context) error {
	now := s.now()
	files, err := filepath.Glob(filepath.Join(s.Dir, "pr-*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		name := filepath.Base(f)
		pr, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "pr-"), ".json"))
		if err != nil {
			return fmt.Errorf("reviewsweep: record name %s: %w", name, err)
		}
		rec, err := reviewstate.Load(s.Dir, pr)
		if err != nil {
			return err
		}
		st := s.Status(pr)
		act := Next(Input{
			Record:       rec,
			JobQueued:    st.JobQueued,
			JobRunning:   st.JobRunning,
			Waiting:      st.Waiting,
			OpenPoints:   rec.OpenPoints,
			AlreadyAsked: st.AlreadyAsked,
		})
		switch act {
		case ActionEnqueueReviewer:
			q, err := reviewroute.Queue(rec.ReviewerModel, s.Cfg)
			if err != nil {
				return err
			}
			m := reviewMessage(rec.PR, rec.Branch, rec.ReviewerModel, now)
			if err := s.Publish(ctx, m, q); err != nil {
				return err
			}
			if err := reviewstate.AppendRevival(s.Dir, reviewstate.Revival{
				At:       now,
				PR:       rec.PR,
				State:    "waiting",
				Enqueued: q,
			}); err != nil {
				return err
			}
			log.Printf("review-sweep: PR %d revived: published %s to %s", rec.PR, m.ID, q)
		case ActionEnqueueAuthor:
			if err := s.EnqueueAuthor(ctx, rec); err != nil {
				return err
			}
			if err := reviewstate.AppendRevival(s.Dir, reviewstate.Revival{
				At:       now,
				PR:       rec.PR,
				State:    "waiting",
				Enqueued: "author job",
			}); err != nil {
				return err
			}
			log.Printf("review-sweep: PR %d revived: author job enqueued", rec.PR)
		case ActionAskNathan:
			log.Printf("review-sweep: PR %d needs Nathan (ask-nathan)", rec.PR)
		}
	}
	return nil
}

// reviewMessage builds the revival message: the same body Job 4 publishes
// (the PR number, the branch, the reviewer), so the review worker sees one
// shape from both paths.
func reviewMessage(pr int, branch, reviewer string, now time.Time) queue.Message {
	return queue.Message{
		ID:        newMessageID(now),
		Type:      "event",
		Priority:  1,
		CreatedAt: now.UTC(),
		Source:    "jarvis-review",
		Title:     fmt.Sprintf("review PR #%d", pr),
		Payload: map[string]string{
			"pr":       strconv.Itoa(pr),
			"branch":   branch,
			"reviewer": reviewer,
		},
	}
}

// newMessageID follows the jarvis-mq message id style
// (mq-<unix>-<three hex bytes>).
func newMessageID(now time.Time) string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "mq-" + strconv.FormatInt(now.UnixNano(), 10)
	}
	return "mq-" + strconv.FormatInt(now.Unix(), 10) + "-" + hex.EncodeToString(b)
}

func (s *Sweep) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
