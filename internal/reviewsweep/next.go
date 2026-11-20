// Package reviewsweep is the five-minute check (Job 10 of
// docs/pr-review-build-plan.md): a pass over the review records that
// decides what is missing for each pull request, revives the missing next
// job, and sends the end of day digest. The process is not a model — it
// publishes jobs and writes logs, it never runs one itself.
package reviewsweep

import (
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// Action is what the five-minute check wants done for one record.
type Action string

// The actions Next can return. ReleaseNever is part of the action space
// but is not returned for Fable: the check does not release a held review
// (Job 4b owns Release), it only makes sure Nathan sees the held job.
const (
	ActionNone            Action = "none"
	ActionEnqueueReviewer Action = "enqueue-reviewer"
	ActionEnqueueAuthor   Action = "enqueue-author"
	ActionAskNathan       Action = "ask-nathan"
	ActionReleaseNever    Action = "release-never"
)

// waitThreshold is how long a missing next job may wait before the check
// acts on it: the sweep runs every five minutes, so a record that has been
// untouched for five minutes with nothing queued is stuck.
const waitThreshold = 5 * time.Minute

// maxRounds and maxOpenPoints are the table's ask-Nathan thresholds
// (config/review.yaml: max_rounds 3, max_open_points 10).
const (
	maxRounds     = 3
	maxOpenPoints = 10
)

// Input is everything Next needs to decide for one record.
type Input struct {
	Record reviewstate.Record
	// JobQueued is true when the expected next job for the PR is already
	// in a queue; JobRunning when it is being worked.
	JobQueued  bool
	JobRunning bool
	// Waiting is how long the record has waited in its current state.
	Waiting time.Duration
	// OpenPoints is the record's open point count.
	OpenPoints int
	// AlreadyAsked is true when a Plane comment asking Nathan about this
	// record already exists.
	AlreadyAsked bool
}

// Next decides what the five-minute check does for in. The table (Job 10a
// of docs/pr-review-build-plan.md), in priority order:
//
//	job queued                                              none
//	job running                                             none
//	waiting 1 minute, nothing queued                        none
//	token is the reviewer, not held, waiting 5 minutes      enqueue-reviewer
//	token is the author, waiting 5 minutes                  enqueue-author
//	rounds >= 3, nothing queued                             ask-nathan
//	open points > 10                                        ask-nathan
//	Held true, waiting 5 minutes                            ask-nathan
//
// A held record never enqueues the reviewer: the held row is decided
// before the token rows, so a held record whose token is the reviewer
// (the situation that would otherwise enqueue it) asks Nathan instead.
// The ask is so Nathan sees a held job that nobody mentioned; when a
// Plane comment asking him already exists the caller passes
// AlreadyAsked and the action is none.
func Next(in Input) Action {
	if in.JobQueued {
		return ActionNone
	}
	if in.JobRunning {
		return ActionNone
	}
	if in.Waiting < waitThreshold {
		return ActionNone
	}
	rec := in.Record
	switch {
	case rec.Held:
		if in.AlreadyAsked {
			return ActionNone
		}
		return ActionAskNathan
	case rec.Token != "" && rec.Token == rec.ReviewerModel:
		return ActionEnqueueReviewer
	case rec.Token != "" && rec.Token == rec.AuthorModel:
		return ActionEnqueueAuthor
	case rec.Rounds >= maxRounds:
		return ActionAskNathan
	case in.OpenPoints > maxOpenPoints:
		return ActionAskNathan
	}
	return ActionNone
}
