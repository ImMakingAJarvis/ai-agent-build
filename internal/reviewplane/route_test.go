package reviewplane

import (
	"testing"
)

// TestRoute is the four-row table from Job 11b of
// docs/pr-review-build-plan.md, plus the row-2 condition made explicit: a
// body that names the author itself is still a guess. No Plane calls.
func TestRoute(t *testing.T) {
	// The map from Plane user id to model id. user-nathan is on purpose
	// not in it: Nathan is not a model.
	userModels := map[string]string{
		"user-sonnet": "sonnet",
		"user-opus":   "opus",
		"user-fable":  "fable",
		"user-local":  "qwen3.8-27b",
	}
	rows := []struct {
		name        string
		comment     Comment
		authorModel string
		namedModel  string
		wantTarget  string
		wantGuess   bool
	}{
		{
			name:        "reply to Opus's user id",
			comment:     Comment{AuthorID: "user-local", ParentAuthorID: "user-opus", Body: "fixed the gate"},
			authorModel: "qwen3.8-27b",
			wantTarget:  "opus",
		},
		{
			name:        "body names sonnet, not the author model",
			comment:     Comment{AuthorID: "user-nathan", Body: "sonnet please look"},
			authorModel: "qwen3.8-27b",
			namedModel:  "sonnet",
			wantTarget:  "sonnet",
		},
		{
			name:        "no reply parent, body names nothing",
			comment:     Comment{AuthorID: "user-nathan", Body: "looks fine"},
			authorModel: "qwen3.8-27b",
			wantTarget:  "qwen3.8-27b",
			wantGuess:   true,
		},
		{
			name:        "reply to Nathan's user id, body empty",
			comment:     Comment{AuthorID: "user-local", ParentAuthorID: "user-nathan", Body: ""},
			authorModel: "qwen3.8-27b",
			wantTarget:  "qwen3.8-27b",
			wantGuess:   true,
		},
		{
			name:        "body names the author model itself",
			comment:     Comment{AuthorID: "user-nathan", Body: "qwen3.8-27b what do you think"},
			authorModel: "qwen3.8-27b",
			namedModel:  "qwen3.8-27b",
			wantTarget:  "qwen3.8-27b",
			wantGuess:   true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, guessed := Route(row.comment, userModels, row.authorModel, row.namedModel)
			if got != row.wantTarget || guessed != row.wantGuess {
				t.Errorf("Route = (%q, %v), want (%q, %v)", got, guessed, row.wantTarget, row.wantGuess)
			}
		})
	}
}
