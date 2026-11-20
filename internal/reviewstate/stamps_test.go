package reviewstate

import (
	"strings"
	"testing"
)

// TestStamp is the Job 2b table, copied row for row.
func TestStamp(t *testing.T) {
	const author, reviewer = "qwen3.8-27b", "sonnet"
	base := Record{PR: 268, Branch: "phase-pr-review-queues", AuthorModel: author, ReviewerModel: reviewer}

	tests := []struct {
		name      string
		start     Record
		call      func(Record) (Record, error)
		wantErr   string // substring the error must contain; empty on success
		unchanged bool   // on error, the record must come back unchanged
		want      func(t *testing.T, got Record)
	}{
		{
			name:  "token author, head aaa, both stamps aaa: ApplyPush by author to bbb",
			start: Record{Token: author, Head: "aaa", AuthorStamp: "aaa", ReviewerStamp: "aaa"},
			call: func(r Record) (Record, error) {
				return ApplyPush(r, author, "bbb")
			},
			want: func(t *testing.T, got Record) {
				if got.AuthorStamp != "bbb" {
					t.Errorf("author stamp = %q, want bbb", got.AuthorStamp)
				}
				if got.ReviewerStamp != "" {
					t.Errorf("reviewer stamp = %q, want empty", got.ReviewerStamp)
				}
				if got.Head != "bbb" {
					t.Errorf("head = %q, want bbb", got.Head)
				}
				if !got.TurnPushed {
					t.Error("TurnPushed = false, want true")
				}
			},
		},
		{
			name:  "token reviewer, head aaa: ApplyPush by reviewer to ccc",
			start: Record{Token: reviewer, Head: "aaa"},
			call: func(r Record) (Record, error) {
				return ApplyPush(r, reviewer, "ccc")
			},
			want: func(t *testing.T, got Record) {
				if got.ReviewerStamp != "ccc" {
					t.Errorf("reviewer stamp = %q, want ccc", got.ReviewerStamp)
				}
				if got.AuthorStamp != "" {
					t.Errorf("author stamp = %q, want empty", got.AuthorStamp)
				}
				if got.Head != "ccc" {
					t.Errorf("head = %q, want ccc", got.Head)
				}
				if !got.TurnPushed {
					t.Error("TurnPushed = false, want true")
				}
			},
		},
		{
			name:      "token author: ApplyPush by reviewer",
			start:     Record{Token: author, Head: "aaa"},
			unchanged: true,
			wantErr:   "token",
			call: func(r Record) (Record, error) {
				return ApplyPush(r, reviewer, "ddd")
			},
		},
		{
			name:  "token author, TurnPushed false, head bbb, author stamp empty: ApplyStamp author bbb",
			start: Record{Token: author, TurnPushed: false, Head: "bbb", AuthorStamp: ""},
			call: func(r Record) (Record, error) {
				return ApplyStamp(r, author, "bbb")
			},
			want: func(t *testing.T, got Record) {
				if got.AuthorStamp != "bbb" {
					t.Errorf("author stamp = %q, want bbb", got.AuthorStamp)
				}
			},
		},
		{
			name:    "token reviewer, TurnPushed true: ApplyStamp reviewer bbb",
			start:   Record{Token: reviewer, TurnPushed: true, Head: "bbb"},
			wantErr: "pushed",
			call: func(r Record) (Record, error) {
				return ApplyStamp(r, reviewer, "bbb")
			},
		},
		{
			name:    "token author, head bbb: ApplyStamp author aaa",
			start:   Record{Token: author, Head: "bbb"},
			wantErr: "head",
			call: func(r Record) (Record, error) {
				return ApplyStamp(r, author, "aaa")
			},
		},
		{
			name:  "token author, TurnPushed true: PassToken",
			start: Record{Token: author, TurnPushed: true, Head: "bbb"},
			call: func(r Record) (Record, error) {
				return PassToken(r), nil
			},
			want: func(t *testing.T, got Record) {
				if got.Token != reviewer {
					t.Errorf("token = %q, want the reviewer %q", got.Token, reviewer)
				}
				if got.TurnPushed {
					t.Error("TurnPushed = true, want false")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// Merge the row's fields over the shared base record.
			row := tt.start
			row.PR = base.PR
			row.Branch = base.Branch
			row.AuthorModel = base.AuthorModel
			row.ReviewerModel = base.ReviewerModel

			got, err := tt.call(row)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				if tt.unchanged && got != row {
					t.Errorf("record changed on error:\n got %+v\nwant %+v", got, row)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.want(t, got)
		})
	}
}
