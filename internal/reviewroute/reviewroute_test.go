package reviewroute

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
)

// fixtureConfig is the config behind the Job 3 routing table: the three
// cloud reviewers, the local fallback, and the three local model ids.
func fixtureConfig() reviewcfg.Config {
	return reviewcfg.Config{
		Reviewers: map[string]reviewcfg.Reviewer{
			"sonnet":             {Queue: "review.sonnet", Kind: "cloud"},
			"opus":               {Queue: "review.opus", Kind: "cloud"},
			"fable":              {Queue: "review.fable", Kind: "cloud", NeedsRelease: true},
			"local":              {Queue: "review.local", Kind: "local"},
			"qwen3.8-27b":        {Queue: "review.local", Kind: "local"},
			"qwen3.8-flash-next": {Queue: "review.local", Kind: "local"},
			"glm5.3-flash":       {Queue: "review.local", Kind: "local"},
		},
	}
}

// TestQueue is the routing table from Job 3b of
// docs/pr-review-build-plan.md, plus one row that a local id not in the
// config returns an error.
func TestQueue(t *testing.T) {
	cfg := fixtureConfig()
	rows := []struct {
		reviewer string
		want     string
		wantErr  bool
	}{
		{reviewer: "sonnet", want: "review.sonnet"},
		{reviewer: "opus", want: "review.opus"},
		{reviewer: "planner", want: "review.opus"},
		{reviewer: "fable", want: "review.fable"},
		{reviewer: "local", want: "review.local"},
		{reviewer: "qwen3.8-27b", want: "review.local"},
		{reviewer: "qwen3.8-flash-next", want: "review.local"},
		{reviewer: "glm5.3-flash", want: "review.local"},
		{reviewer: "gpt-5.2", wantErr: true},     // anything else
		{reviewer: "qwen4.1-32b", wantErr: true}, // a local id not in the config
	}
	for _, row := range rows {
		got, err := Queue(row.reviewer, cfg)
		if row.wantErr {
			if err == nil {
				t.Errorf("Queue(%q) = %q, want error", row.reviewer, got)
				continue
			}
			if !strings.Contains(err.Error(), row.reviewer) {
				t.Errorf("Queue(%q) error %q does not contain the reviewer", row.reviewer, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Queue(%q) unexpected error: %v", row.reviewer, err)
			continue
		}
		if got != row.want {
			t.Errorf("Queue(%q) = %q, want %q", row.reviewer, got, row.want)
		}
	}
}
