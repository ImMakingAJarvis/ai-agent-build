// Package reviewroute maps a reviewer id to the review queue that carries
// its jobs (Job 3 of docs/pr-review-build-plan.md). The mapping comes from
// the loaded review config (config/review.yaml), not a hard-coded list: a
// new local model id added there under kind: local routes to review.local
// without a code change.
package reviewroute

import (
	"fmt"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
)

// planner is an alias for opus: jarvis-review.sh already accepts it (the
// stronger planning review for docs/plan-only PRs), so the opus queue
// carries it.
const plannerAlias = "planner"

// Queue returns the review queue for the reviewer id, from the loaded
// review config. "planner" routes to the opus queue. An id that is not a
// reviewer in the config — including a local model id Nathan has not added
// yet — returns an error that names the reviewer.
func Queue(reviewer string, cfg reviewcfg.Config) (string, error) {
	if reviewer == plannerAlias {
		reviewer = "opus"
	}
	r, ok := cfg.Reviewers[reviewer]
	if !ok {
		return "", fmt.Errorf("unknown reviewer %q: not in the review config", reviewer)
	}
	return r.Queue, nil
}
