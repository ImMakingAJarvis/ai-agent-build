// Where the chat daemon finds the review config (config/review.yaml):
// the !fullauto command validates the reviewer it is named against it,
// and the !review command routes through it (PR review queues build,
// Job 5 of docs/pr-review-build-plan.md).
package main

import (
	"os"
	"path/filepath"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
)

// loadReviewConfig reads the review config. Path resolution: $REVIEW_CONFIG
// (tests), then /config/review.yaml (the compose mount into the chat
// container), then config/review.yaml (a checkout, when the daemon is run
// from the repo root).
func loadReviewConfig() (reviewcfg.Config, error) {
	path := os.Getenv("REVIEW_CONFIG")
	if path == "" {
		if _, err := os.Stat("/config/review.yaml"); err == nil {
			path = "/config/review.yaml"
		} else {
			path = filepath.Join("config", "review.yaml")
		}
	}
	return reviewcfg.Load(path)
}
