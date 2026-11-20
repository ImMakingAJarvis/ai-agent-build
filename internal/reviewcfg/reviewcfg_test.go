package reviewcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// job1aYAML is the file from Job 1a of docs/pr-review-build-plan.md,
// verbatim.
const job1aYAML = `# Review queues. Loaded by internal/reviewcfg.
month_cap_usd: 10
max_open_prs: 2
max_rounds: 3
max_open_points: 10
# Empty until Nathan adds a model id. No model merges to main while this is empty.
merge_allow: []
# Author response jobs use class build at this priority (higher runs first
# inside the build band, and every fix still runs before any build).
author_job_class: build
author_job_priority: 500
prices_usd_per_mtok:
  sonnet: 3
  opus: 15
  fable: 10
reviewers:
  sonnet:
    queue: review.sonnet
    kind: cloud
    needs_release: false
  opus:
    queue: review.opus
    kind: cloud
    needs_release: false
  fable:
    queue: review.fable
    kind: cloud
    needs_release: true
  qwen3.8-27b:
    queue: review.local
    kind: local
    needs_release: false
sensitive_paths:
  - cmd/resourced/
  - internal/streamwatcher/
  - scripts/jarvis-watchdog
  - scripts/voice_service.py
  - scripts/jarvis-voice.service
  - internal/dashboard/
  - cmd/dashboard/
  - internal/secrets/
  - scripts/jarvis-secrets
  - docker-compose.yml
  - scripts/agent-run
`

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "review.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return p
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantErr   string // substring the error must contain; "" means no error
		wantCheck func(t *testing.T, c Config)
	}{
		{
			name:    "job 1a file",
			content: job1aYAML,
			wantCheck: func(t *testing.T, c Config) {
				if c.MonthCapUSD != 10 {
					t.Errorf("MonthCapUSD = %v, want 10", c.MonthCapUSD)
				}
				if len(c.Reviewers) != 4 {
					t.Errorf("len(Reviewers) = %d, want 4", len(c.Reviewers))
				}
				if !c.Reviewers["fable"].NeedsRelease {
					t.Errorf("fable NeedsRelease = false, want true")
				}
				if len(c.MergeAllow) != 0 {
					t.Errorf("len(MergeAllow) = %d, want 0", len(c.MergeAllow))
				}
			},
		},
		{
			name:    "zero month cap",
			content: strings.Replace(job1aYAML, "month_cap_usd: 10", "month_cap_usd: 0", 1),
			wantErr: "month_cap_usd",
		},
		{
			name:    "duplicate queue",
			content: strings.Replace(job1aYAML, "queue: review.opus", "queue: review.sonnet", 1),
			wantErr: "queue",
		},
		{
			name:    "merge allow sonnet",
			content: strings.Replace(job1aYAML, "merge_allow: []", "merge_allow: [sonnet]", 1),
			wantCheck: func(t *testing.T, c Config) {
				if len(c.MergeAllow) != 1 || c.MergeAllow[0] != "sonnet" {
					t.Errorf("MergeAllow = %v, want [sonnet]", c.MergeAllow)
				}
			},
		},
		{
			name:    "zero max open prs",
			content: strings.Replace(job1aYAML, "max_open_prs: 2", "max_open_prs: 0", 1),
			wantErr: "max_open_prs",
		},
		{
			name:    "zero max rounds",
			content: strings.Replace(job1aYAML, "max_rounds: 3", "max_rounds: 0", 1),
			wantErr: "max_rounds",
		},
		{
			name:    "zero max open points",
			content: strings.Replace(job1aYAML, "max_open_points: 10", "max_open_points: 0", 1),
			wantErr: "max_open_points",
		},
		{
			name:    "empty reviewer queue",
			content: strings.Replace(job1aYAML, "queue: review.fable", "queue: \"\"", 1),
			wantErr: "queue",
		},
		{
			name:    "wrong author job class",
			content: strings.Replace(job1aYAML, "author_job_class: build", "author_job_class: fix", 1),
			wantErr: "author_job_class",
		},
		{
			name:    "empty sensitive path",
			content: strings.Replace(job1aYAML, "- scripts/agent-run", "- \"\"", 1),
			wantErr: "sensitive_paths",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(writeYAML(t, tt.content))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.wantCheck != nil {
				tt.wantCheck(t, c)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}

// TestLoadRepoConfig loads the real config/review.yaml so the Job 1a
// constant above cannot drift from the committed file.
func TestLoadRepoConfig(t *testing.T) {
	p := filepath.Join("..", "..", "config", "review.yaml")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load(%s): %v", p, err)
	}
	if len(c.Reviewers) != 4 || len(c.MergeAllow) != 0 {
		t.Errorf("repo config: %d reviewers, merge_allow %v — want 4 reviewers, empty allow list", len(c.Reviewers), c.MergeAllow)
	}
	if got := c.PricesUSDPerMTok["opus"]; got == 0 {
		t.Error("repo config: opus price is 0 — a zero would bypass the month cap")
	}
}
