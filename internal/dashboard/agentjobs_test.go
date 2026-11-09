package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// parseTemplates parses all dashboard templates with the server's function map.
func parseTemplates(t *testing.T) (*template.Template, error) {
	t.Helper()
	return template.New("").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
}

// TestIndexIsALightShell ensures the page is a static shell that loads every
// data section lazily — the paginated lists (approvals, agent jobs, recent
// jobs) load once and page in via "Load more", so they must not poll.
func TestIndexIsALightShell(t *testing.T) {
	tmpl, err := template.New("index.html").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "index.html", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"Loading queue status", "/api/approvals", "/html/agent-jobs-section", "/html/jobs-section", "page of 25"} {
		if !strings.Contains(out, w) {
			t.Errorf("shell lacks %q", w)
		}
	}
	for _, id := range []string{`id="jobs-section"`, `id="agent-jobs-section"`, `id="approvals"`} {
		i := strings.Index(out, id)
		if i < 0 {
			t.Errorf("shell lacks %q", id)
			continue
		}
		tag := out[i : i+strings.IndexByte(out[i:], '>')]
		if strings.Contains(tag, "every ") {
			t.Errorf("%q still polls; paginated sections must load once", id)
		}
	}
}

// TestAgentJobsSectionPaginates checks the paginated agent-job list: rows
// with log links and power, the store meta line, and the "Load more" row.
func TestAgentJobsSectionPaginates(t *testing.T) {
	tmpl, err := template.New("agent-jobs-section").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	page := AgentJobsPage{
		Items: []agentjob.Job{{
			Spec:  agentjob.Spec{ID: "d0", Title: "Fix poll", DependsOn: []string{"j0"}, Driver: "local"},
			State: agentjob.State{Status: agentjob.Done, RanWith: "sonnet", PRURL: "https://github.com/o/r/pull/9", Kwh: 0.5, PowerCost: 0.01},
		}},
		WorkerMessages: 1,
		ListPage:       ListPage{Total: 80, Shown: 25, HasMore: true, Hidden: 55, NextOffset: 25},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "agent-jobs-section", page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"d0 — Fix poll", "done", "sonnet", "pull/9", `href="/jobs/d0"`, ">Log<", "80 jobs", "Load 25 more", "1 message(s)", "?offset=25"} {
		if !strings.Contains(out, w) {
			t.Errorf("section lacks %q", w)
		}
	}
}

// TestAgentJobsSectionWaitingRow: a waiting job renders as "waiting on
// <corrs> (<age>)" instead of the bare status, and a ready job next to it
// keeps its plain status.
func TestAgentJobsSectionWaitingRow(t *testing.T) {
	tmpl, err := template.New("agent-jobs-section").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	page := AgentJobsPage{
		Items: []agentjob.Job{
			{
				Spec:  agentjob.Spec{ID: "w0", Title: "Make photo", WaitOn: []string{"J-img-1", "J-img-2"}, Driver: "local"},
				State: agentjob.State{Status: agentjob.Waiting, RanWith: "jarvis-brain", WaitingSince: time.Now().UTC().Format(time.RFC3339)},
			},
			{
				Spec:  agentjob.Spec{ID: "r0", Title: "Next up", Driver: "local"},
				State: agentjob.State{Status: agentjob.Ready},
			},
		},
		ListPage: ListPage{Total: 2, Shown: 2},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "agent-jobs-section", page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "waiting on J-img-1, J-img-2 (") {
		t.Errorf("waiting row lacks the wait-on text: %s", out)
	}
	if !strings.Contains(out, ">ready<") {
		t.Errorf("ready row lost its plain status: %s", out)
	}
}

// TestJobsSectionPaginates checks the cost cards (including power/energy)
// and the paginated recent-jobs table.
func TestJobsSectionPaginates(t *testing.T) {
	tmpl, err := template.New("jobs-section").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	page := RecentJobsPage{
		Items: []JobRecord{{
			ID: "j1", Type: "build", Model: "qwen", Queue: "agent",
			EstimatedCost: 0.42, Status: "done", StartedAt: "2026-11-08T10:00:00Z",
			Duration: "3m", PowerCost: 0.01, Kwh: 0.5, UseLocal: true,
		}},
		TotalCost: 0.42, TotalPowerCost: 0.01, TotalKwh: 0.5, APICount: 1, LocalCount: 1,
		ListPage: ListPage{Total: 40, Shown: 25, HasMore: true, Hidden: 15, NextOffset: 25},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "jobs-section", page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"40 distinct jobs", "Load 15 more", "Power Cost (local)", "Energy (local)", ">j1<", `href="/jobs/j1"`, "?offset=25"} {
		if !strings.Contains(out, w) {
			t.Errorf("section lacks %q", w)
		}
	}
}

// TestApprovalsPaginates checks the denied-job list: cards with approve/
// reject actions, the meta line, and the "Load more" row.
func TestApprovalsPaginates(t *testing.T) {
	tmpl, err := template.New("approvals").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	page := ApprovalPage{
		Items: []ApprovalInfo{{
			ID: "j9", Type: "build", Queue: "agent", Model: "claude",
			EstimatedCost: 1.5, Source: "discord", StartedAt: "2026-11-08T10:00:00Z",
		}},
		ListPage: ListPage{Total: 30, Shown: 25, HasMore: true, Hidden: 5, NextOffset: 25},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "approvals", page); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"30 denied", "Load 5 more", `id="approvals"`, "action=approve", "action=reject", `href="/jobs/j9"`, "?offset=25"} {
		if !strings.Contains(out, w) {
			t.Errorf("section lacks %q", w)
		}
	}
}
