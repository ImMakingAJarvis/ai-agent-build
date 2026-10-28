package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func parseTemplates(t *testing.T) (*template.Template, error) {
	t.Helper()
	funcs := template.FuncMap{"statusClass": StatusClass, "formatDuration": FormatDuration, "formatCost": FormatCost, "formatKwh": FormatKwh, "divCost": DivCost, "slotRuntime": SlotRuntime, "jobPath": JobPath}
	return template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
}

func TestAgentJobsSection(t *testing.T) {
	tmpl, err := parseTemplates(t)
	if err != nil {
		t.Fatal(err)
	}
	st := DashboardState{AgentJobs: []agentjob.Job{{
		Spec:  agentjob.Spec{ID: "d0", Title: "Fix poll", DependsOn: []string{"j0"}, Driver: "local"},
		State: agentjob.State{Status: agentjob.Done, RanWith: "sonnet", PRURL: "https://github.com/o/r/pull/9", Kwh: 0.5, PowerCost: 0.075},
	}}}
	st.InProgress, st.Waiting, st.WaitingMore, st.Finished, st.FinishedTotal = splitAgentJobs(st.AgentJobs)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "agent-jobs-section", st); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"d0 — Fix poll", "done", "sonnet", "pull/9", "j0", `href="/jobs/d0"`, ">Log<", "0.500 kWh", "$0.0750"} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("page lacks %q", w)
		}
	}
}
