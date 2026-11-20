package reviewturn

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// TestAuthorJob covers Job 7c: the author response job is a 500-priority
// build (not fix), its title carries the PR number, and its instructions
// tell the author what to read, where to reply, and what it must not do.
func TestAuthorJob(t *testing.T) {
	r := reviewstate.Record{
		PR: 42, Branch: "feature-x", Head: "bbb",
		AuthorModel: "local", ReviewerModel: "sonnet", Token: "sonnet",
		AuthorStamp: "aaa", ReviewerStamp: "bbb", PlaneItem: "REV-4",
	}

	spec := AuthorJob(r)

	if spec.Class != agentjob.ClassBuild {
		t.Errorf("class = %q, want %q (not %q)", spec.Class, agentjob.ClassBuild, agentjob.ClassFix)
	}
	if spec.Priority != 500 {
		t.Errorf("priority = %d, want 500", spec.Priority)
	}
	if !strings.Contains(spec.Title, "#42") {
		t.Errorf("title %q does not contain the PR number", spec.Title)
	}
	for _, want := range []string{
		"since your last stamp", // read the commits added since its last stamp
		"REV-4",                 // reply on the Plane item
		"stamp",                 // stamp the head with no push
		"push a correction",     // or push a correction
		"Do not merge to main",  // not merge to main
	} {
		if !strings.Contains(spec.Instructions, want) {
			t.Errorf("instructions missing %q:\n%s", want, spec.Instructions)
		}
	}
}
