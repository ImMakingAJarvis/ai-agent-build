package reviewsweep

import (
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// TestNext covers the Job 10a table in docs/pr-review-build-plan.md, row by
// row. waiting is five minutes unless the row says one minute.
func TestNext(t *testing.T) {
	one := time.Minute
	five := 5 * time.Minute

	// A record whose token is the reviewer: the held row below uses the same
	// record, so that row is "would otherwise enqueue the reviewer".
	reviewerRec := reviewstate.Record{
		PR: 268, Branch: "phase-x", Head: "abc123",
		ReviewerModel: "sonnet", Token: "sonnet",
	}
	authorRec := reviewstate.Record{
		PR: 268, Branch: "phase-x", Head: "abc123",
		AuthorModel: "local", Token: "local",
	}
	roundsRec := reviewstate.Record{
		PR: 268, Branch: "phase-x", Rounds: 3,
	}
	pointsRec := reviewstate.Record{
		PR: 268, Branch: "phase-x",
	}
	heldRec := reviewstate.Record{
		PR: 268, Branch: "phase-x",
		ReviewerModel: "fable", Token: "fable", Held: true,
	}

	tests := []struct {
		name  string
		in    Input
		want  Action
	}{
		{
			name: "job queued",
			in:   Input{Record: reviewerRec, JobQueued: true, Waiting: five},
			want: ActionNone,
		},
		{
			name: "job running",
			in:   Input{Record: reviewerRec, JobRunning: true, Waiting: five},
			want: ActionNone,
		},
		{
			name: "waiting one minute nothing queued",
			in:   Input{Record: reviewerRec, Waiting: one},
			want: ActionNone,
		},
		{
			name: "token is the reviewer not held waiting five minutes",
			in:   Input{Record: reviewerRec, Waiting: five},
			want: ActionEnqueueReviewer,
		},
		{
			name: "token is the author nothing queued waiting five minutes",
			in:   Input{Record: authorRec, Waiting: five},
			want: ActionEnqueueAuthor,
		},
		{
			name: "rounds at the cap nothing queued",
			in:   Input{Record: roundsRec, Waiting: five},
			want: ActionAskNathan,
		},
		{
			name: "open points over the cap",
			in:   Input{Record: pointsRec, OpenPoints: 11, Waiting: five},
			want: ActionAskNathan,
		},
		{
			name: "held waiting five minutes",
			in:   Input{Record: heldRec, Waiting: five},
			want: ActionAskNathan,
		},
		{
			name: "held token is the reviewer: never enqueue the reviewer",
			in:   Input{Record: heldRec, Waiting: five},
			want: ActionAskNathan,
		},
		{
			name: "held and a Plane comment already asked Nathan",
			in:   Input{Record: heldRec, AlreadyAsked: true, Waiting: five},
			want: ActionNone,
		},
		{
			name: "nothing applies",
			in:   Input{Record: pointsRec, Waiting: five},
			want: ActionNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Next(tt.in); got != tt.want {
				t.Errorf("Next(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
