package reviewturn

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// ready is a record where head and both stamps are "abc" and the reviewer
// holds the token: every readiness check passes.
func ready(reviewer string) reviewstate.Record {
	return reviewstate.Record{
		Head:          "abc",
		AuthorModel:   "local",
		ReviewerModel: reviewer,
		Token:         reviewer,
		AuthorStamp:   "abc",
		ReviewerStamp: "abc",
	}
}

// TestMerge covers Job 8a: the rows are copied from the table in
// docs/pr-review-build-plan.md. allow is ["sonnet"] unless the row says
// otherwise.
func TestMerge(t *testing.T) {
	rows := []struct {
		name    string
		record  reviewstate.Record
		actor   string
		pushed  bool
		allow   []string
		want    Kind
		wantSub string // the reason must contain this
	}{
		{
			name:    "both stamps name head, token reviewer, reviewer on the allow list",
			record:  ready("sonnet"),
			actor:   "sonnet",
			allow:   []string{"sonnet"},
			want:    Merge,
			wantSub: "",
		},
		{
			name: "reviewer stamp empty",
			record: func() reviewstate.Record {
				r := ready("sonnet")
				r.ReviewerStamp = ""
				return r
			}(),
			actor:   "sonnet",
			allow:   []string{"sonnet"},
			want:    Refuse,
			wantSub: "stamp",
		},
		{
			name:    "both stamps name head, but this turn pushed",
			record:  ready("sonnet"),
			actor:   "sonnet",
			pushed:  true,
			allow:   []string{"sonnet"},
			want:    Refuse,
			wantSub: "pushed",
		},
		{
			name: "both stamps name a different sha than the head",
			record: func() reviewstate.Record {
				r := ready("sonnet")
				r.AuthorStamp = "abd"
				r.ReviewerStamp = "abd"
				return r
			}(),
			actor:   "sonnet",
			allow:   []string{"sonnet"},
			want:    Refuse,
			wantSub: "head",
		},
		{
			name: "token held by the author",
			record: func() reviewstate.Record {
				r := ready("sonnet")
				r.Token = r.AuthorModel
				return r
			}(),
			actor:   "sonnet",
			allow:   []string{"sonnet"},
			want:    Refuse,
			wantSub: "token",
		},
		{
			name:    "both stamps name head, allow list empty",
			record:  ready("sonnet"),
			actor:   "sonnet",
			allow:   []string{},
			want:    Ask,
			wantSub: "allow",
		},
		{
			name: "reviewer is fable and the record is held",
			record: func() reviewstate.Record {
				r := ready("fable")
				r.Held = true
				return r
			}(),
			actor:   "fable",
			allow:   []string{"fable"},
			want:    Ask,
			wantSub: "release",
		},
		{
			name: "held row: held always asks, even on the allow list",
			record: func() reviewstate.Record {
				r := ready("fable")
				r.Held = true
				return r
			}(),
			actor:   "fable",
			allow:   []string{"fable"},
			want:    Ask,
			wantSub: "held",
		},
		{
			name:    "reviewer is fable, not held, on the allow list",
			record:  ready("fable"),
			actor:   "fable",
			allow:   []string{"fable"},
			want:    Merge,
			wantSub: "",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d := Decide(row.record, row.actor, row.allow, row.pushed)
			if d.Kind != row.want {
				t.Fatalf("Decide = %q (%q), want %q", d.Kind, d.Reason, row.want)
			}
			if row.want == Merge {
				if d.Reason != "" {
					t.Errorf("merge reason = %q, want empty", d.Reason)
				}
				return
			}
			if !strings.Contains(d.Reason, row.wantSub) {
				t.Errorf("reason %q does not contain %q", d.Reason, row.wantSub)
			}
		})
	}
}
