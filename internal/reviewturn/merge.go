// Merge gate (Job 8 of docs/pr-review-build-plan.md): main updates only
// when both stamps name the current head, this turn did not push, the
// actor holds the turn token, the record is not held, and the actor is on
// merge_allow. Anything not ready is a refuse (no merge, no question to
// Nathan — the next model still gets the token from the caller); anything
// ready but not allowed is an ask (a Plane comment to Nathan, no merge).
package reviewturn

import (
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// Kind is the outcome of a merge decision.
type Kind string

// The three outcomes.
const (
	// Merge: update main now.
	Merge Kind = "merge"
	// Ask: post a Plane comment to Nathan and do not merge.
	Ask Kind = "ask"
	// Refuse: the stamps are simply not ready; no merge and no question.
	Refuse Kind = "refuse"
)

// Decision is the merge-gate outcome for one turn: which outcome applies
// and why.
type Decision struct {
	Kind   Kind
	Reason string
}

// Decide applies the merge gate to one turn. allow is the merge_allow
// list from config/review.yaml (nil or empty means no model may merge).
func Decide(r reviewstate.Record, actor string, allow []string, turnPushed bool) Decision {
	// Readiness: both stamps must be set (refuse, "stamp") and must name
	// the current head (refuse, "head").
	if r.AuthorStamp == "" || r.ReviewerStamp == "" {
		return Decision{Kind: Refuse, Reason: fmt.Sprintf(
			"stamps are not ready: author stamp %q, reviewer stamp %q", r.AuthorStamp, r.ReviewerStamp)}
	}
	if r.AuthorStamp != r.Head || r.ReviewerStamp != r.Head {
		return Decision{Kind: Refuse, Reason: fmt.Sprintf(
			"stamps do not name the current head %q: author stamp %q, reviewer stamp %q",
			r.Head, r.AuthorStamp, r.ReviewerStamp)}
	}
	// A turn that pushed cannot merge on the same turn.
	if turnPushed {
		return Decision{Kind: Refuse, Reason: "this turn pushed, so it cannot merge on the same turn"}
	}
	// Only the model holding the turn token merges.
	if actor != r.Token {
		return Decision{Kind: Refuse, Reason: fmt.Sprintf(
			"%q does not hold the turn token %q", actor, r.Token)}
	}
	// A held record (Fable not released) always asks, even when the
	// reviewer is on the allow list.
	if r.Held {
		return Decision{Kind: Ask, Reason: "record is held: fable is not released yet, so the gate asks Nathan to release it"}
	}
	// The actor must be on merge_allow; an empty list means nobody merges.
	for _, id := range allow {
		if id == actor {
			return Decision{Kind: Merge}
		}
	}
	return Decision{Kind: Ask, Reason: fmt.Sprintf(
		"%q is not in merge_allow (%s), so the gate asks Nathan", actor, strings.Join(allow, ", "))}
}
