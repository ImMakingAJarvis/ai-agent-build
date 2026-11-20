package reviewauto

import (
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
)

// sensitiveFromConfig loads the path list from config/review.yaml —
// the same list the production caller passes in — so the test
// exercises the real list and this package never carries a copy.
func sensitiveFromConfig(t *testing.T) []string {
	t.Helper()
	cfg, err := reviewcfg.Load("../../config/review.yaml")
	if err != nil {
		t.Fatalf("load config/review.yaml: %v", err)
	}
	return cfg.SensitivePaths
}

func TestRisk(t *testing.T) {
	sensitive := sensitiveFromConfig(t)

	t.Run("Rate", func(t *testing.T) {
		cases := []struct {
			name         string
			authorRating string
			files        []string
			want         string
		}{
			{"note stays note on a docs file", "note", []string{"docs/readme.md"}, "note"},
			{"note floors to high under cmd/resourced/", "note", []string{"cmd/resourced/stream.go"}, "high"},
			{"note floors to high under internal/secrets/", "note", []string{"internal/secrets/store.go"}, "high"},
			{"note floors to high on docker-compose.yml", "note", []string{"docker-compose.yml"}, "high"},
			{"medium-low stays medium-low on a docs file", "medium-low", []string{"docs/readme.md"}, "medium-low"},
			{"empty rating is high", "", []string{"docs/readme.md"}, "high"},
			{"unknown rating is high", "banana", []string{"docs/readme.md"}, "high"},
			{"sensitive floor does not lower very high", "very high", []string{"docker-compose.yml"}, "very high"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got := Rate(c.authorRating, c.files, sensitive); got != c.want {
					t.Errorf("Rate(%q, %v) = %q, want %q", c.authorRating, c.files, got, c.want)
				}
			})
		}

		t.Run("file entries match only that file", func(t *testing.T) {
			if got := Rate("note", []string{"scripts/docker-compose.yml.bak"}, sensitive); got != "note" {
				t.Errorf("docker-compose.yml must match only itself, got %q", got)
			}
		})
	})

	t.Run("NeedsReview", func(t *testing.T) {
		if NeedsReview("note") {
			t.Error("NeedsReview(note) = true, want false")
		}
		for _, r := range []string{"medium-low", "medium", "high", "very high", ""} {
			if !NeedsReview(r) {
				t.Errorf("NeedsReview(%q) = false, want true", r)
			}
		}
	})
}
