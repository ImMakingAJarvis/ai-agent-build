package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestIndexIsALightShell(t *testing.T) {
	funcs := jobLogFuncs()
	tmpl, err := template.New("index.html").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, DashboardState{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"Loading queue status", "/html/approvals", "/html/agent-jobs-section", "/html/jobs-section", "page of 25"} {
		if !strings.Contains(out, w) {
			t.Errorf("shell lacks %q", w)
		}
	}
	if strings.Contains(out, "hx-get=\"/html/dashboard\"") && strings.Contains(out, "every 5s") && strings.Contains(out, "id=\"dashboard\" hx-get") {
		t.Error("index should not poll the full dashboard every 5s")
	}
}

func TestAgentJobsSectionHasLogAndPage(t *testing.T) {
	tmpl, err := template.New("agent-jobs-section").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	page := AgentJobsPage{
		Items: []agentjob.Job{{
			Spec:  agentjob.Spec{ID: "d0", Title: "Fix poll", DependsOn: []string{"j0"}, Driver: "local"},
			State: agentjob.State{Status: agentjob.Done, RanWith: "sonnet", PRURL: "https://github.com/o/r/pull/9"},
		}},
		ListPage: ListPage{Total: 80, Shown: 25, HasMore: true, Hidden: 55, NextOffset: 25},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "agent-jobs-section", page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"d0 — Fix poll", "done", "sonnet", "pull/9", `href="/jobs/d0"`, ">Log<", "80 jobs", "Load 25 more"} {
		if !strings.Contains(out, w) {
			t.Errorf("section lacks %q", w)
		}
	}
}
