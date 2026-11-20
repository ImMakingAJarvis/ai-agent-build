package reviewturn

import (
	"fmt"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// AuthorJob builds the job that wakes the PR's author after the reviewer
// pushed: the author reads the commits added since its last stamp, replies
// on the Plane item, and either stamps the head with no push or pushes a
// correction. The class is build at priority 500, so it sits above ordinary
// builds and below every fix; a new job class would have to be threaded
// through DefaultBandOrder, SetBandOrder, and the dashboard.
func AuthorJob(r reviewstate.Record) agentjob.Spec {
	since := r.AuthorStamp
	if since == "" {
		since = "the start of the PR"
	}
	return agentjob.Spec{
		Title:    fmt.Sprintf("PR #%d: respond to the reviewer's commits", r.PR),
		Branch:   r.Branch,
		Class:    agentjob.ClassBuild,
		Priority: 500,
		Instructions: fmt.Sprintf(
			"You are the author of PR #%d on branch %s. "+
			"Read the commits added since your last stamp (%s). "+
			"Reply on the Plane item %s with what you see. "+
			"Either stamp the current head %s with no push, or push a correction to the PR branch. "+
			"Do not merge to main.",
			r.PR, r.Branch, since, r.PlaneItem, r.Head,
		),
	}
}
