package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestIndexRendersAgentJobs(t *testing.T) {
	funcs := template.FuncMap{"statusClass": StatusClass, "formatDuration": FormatDuration, "formatCost": FormatCost, "divCost": DivCost, "slotRuntime": SlotRuntime}
	tmpl, err := template.New("index.html").Funcs(funcs).ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	st := DashboardState{AgentJobs: []agentjob.Job{{
		Spec:  agentjob.Spec{ID: "d0", Title: "Fix poll", DependsOn: []string{"j0"}, Driver: "local"},
		State: agentjob.State{Status: agentjob.Done, RanWith: "sonnet", PRURL: "https://github.com/o/r/pull/9"},
	}}}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, st); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"d0 — Fix poll", "done", "sonnet", "pull/9", "j0"} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("page lacks %q", w)
		}
	}
}
