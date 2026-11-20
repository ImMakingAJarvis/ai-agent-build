// Package reviewturn holds the review-turn push rules (Job 7 of
// docs/pr-review-build-plan.md): only the model holding the turn token may
// push to the pull request branch, a push updates the stamps and ends the
// turn, and the author response job is built from the record.
package reviewturn

import (
	"fmt"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// AllowPush reports whether pusher may push to the pull request branch.
// Only the model holding the turn token may push, and a turn whose token is
// empty allows no push at all.
func AllowPush(r reviewstate.Record, pusher string) error {
	if r.Token == "" {
		return fmt.Errorf("reviewturn: the turn token is empty, so %q may not push", pusher)
	}
	if pusher != r.Token {
		return fmt.Errorf("reviewturn: %q does not hold the turn token %q", pusher, r.Token)
	}
	return nil
}

// Push runs push only when pusher holds the turn token; a failed check
// returns the check's error and never invokes push.
func Push(r reviewstate.Record, pusher string, push func() error) error {
	if err := AllowPush(r, pusher); err != nil {
		return err
	}
	return push()
}
