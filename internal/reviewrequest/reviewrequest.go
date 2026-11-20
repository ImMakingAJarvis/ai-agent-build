// Package reviewrequest is the sender side of a PR review request
// (Job 4 of docs/pr-review-build-plan.md): it saves the reviewstate
// record for a pull request and publishes the review job to the
// reviewer's queue (internal/reviewroute). A reviewer whose config
// entry has needs_release (Fable) is held instead: the record is
// saved with Held true and nothing is published until Release.
package reviewrequest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewroute"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// Service sends review requests. Dir and Cfg come from the wiring;
// Publish delivers the queue message — the real wiring uses
// internal/queue, and tests set a fake (no broker).
type Service struct {
	// Dir is where the reviewstate records live (reviewstate.DefaultDir).
	Dir string
	// Cfg is the loaded review config (config/review.yaml).
	Cfg reviewcfg.Config
	// Publish delivers one message to a review queue by name.
	Publish func(ctx context.Context, m queue.Message, q string) error
}

// RequestReview records that PR pr (branch, head) is requested for
// review by reviewer and returns the job id of what was enqueued and
// whether the request is held.
//
// An unknown reviewer returns the reviewroute error and publishes
// nothing. A known reviewer whose needs_release is true (Fable) is
// held: the record is saved with Held true, nothing is published, and
// the returned job id is stable for the PR (HeldJobID). Any other
// known reviewer gets exactly one message on its queue — unless a
// record already exists for that reviewer with Held false and the
// same head, in which case nothing is published and the returned job
// id is empty (nothing new was enqueued).
//
// The record's Token starts as the reviewer; a new request starts a
// fresh turn, so TurnPushed is reset. A record that already exists
// keeps its AuthorModel and AuthorStamp; on a new record the stamps
// stay empty until the author job sets them. This package never
// guesses the SHA: the caller passes head.
func (s *Service) RequestReview(ctx context.Context, pr int, branch, reviewer, planeItem, head string) (jobID string, held bool, err error) {
	if pr <= 0 {
		return "", false, fmt.Errorf("reviewrequest: bad PR number %d", pr)
	}
	q, err := reviewroute.Queue(reviewer, s.Cfg)
	if err != nil {
		return "", false, err
	}
	var rec reviewstate.Record
	existed := false
	old, err := reviewstate.Load(s.Dir, pr)
	switch {
	case err == nil:
		rec, existed = old, true
	case errors.Is(err, os.ErrNotExist):
	default:
		return "", false, err
	}
	// A review is already in flight for this reviewer at this head.
	if existed && rec.ReviewerModel == reviewer && !rec.Held && rec.Head == head {
		return "", false, nil
	}
	rec.PR = pr
	rec.Branch = branch
	rec.Head = head
	rec.ReviewerModel = reviewer
	rec.Token = reviewer
	rec.PlaneItem = planeItem
	rec.TurnPushed = false
	if s.Cfg.Reviewers[reviewer].NeedsRelease {
		rec.Held = true
		if err := reviewstate.Save(s.Dir, rec); err != nil {
			return "", false, err
		}
		return HeldJobID(pr), true, nil
	}
	m := s.message(pr, branch, reviewer)
	if err := s.Publish(ctx, m, q); err != nil {
		return "", false, err
	}
	rec.Held = false
	if err := reviewstate.Save(s.Dir, rec); err != nil {
		return "", false, err
	}
	return m.ID, false, nil
}

// HeldJobID is the stable job id of the held Fable request for PR pr.
func HeldJobID(pr int) string {
	return fmt.Sprintf("review-held-%d-fable", pr)
}

// Release publishes the one message for a held request and clears
// Held. Releasing a record that is not held returns an error that
// contains "held" and publishes nothing, so a second Release
// publishes nothing.
func (s *Service) Release(ctx context.Context, pr int) error {
	rec, err := reviewstate.Load(s.Dir, pr)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reviewrequest: PR %d has no review record to release", pr)
		}
		return err
	}
	if !rec.Held {
		return fmt.Errorf("reviewrequest: PR %d is not held", pr)
	}
	q, err := reviewroute.Queue(rec.ReviewerModel, s.Cfg)
	if err != nil {
		return err
	}
	m := s.message(rec.PR, rec.Branch, rec.ReviewerModel)
	if err := s.Publish(ctx, m, q); err != nil {
		return err
	}
	rec.Held = false
	return reviewstate.Save(s.Dir, rec)
}

// message builds the review job message: the same body
// scripts/jarvis-review.sh publishes (Job 3) — the PR number, the
// branch and the reviewer — so the review worker sees one shape from
// both paths.
func (s *Service) message(pr int, branch, reviewer string) queue.Message {
	return queue.Message{
		ID:        newMessageID(),
		Type:      "event",
		Priority:  1,
		CreatedAt: time.Now().UTC(),
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
func newMessageID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "mq-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "mq-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(b)
}
