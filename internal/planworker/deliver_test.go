package planworker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// --- fixtures ----------------------------------------------------------------

// deliverRegistry is the fixture registry: one enabled executor, the
// model the toy tasks are planned onto.
func deliverRegistry() *registry.File {
	return &registry.File{Models: []registry.Model{{
		ID: "qwen3.8-27b", Roles: []string{"executor"}, Enabled: true,
	}}}
}

func toyJob() *Job {
	return &Job{
		Trigger: "plan_epic",
		Board:   Board{URL: "http://plane.test", Workspace: "testws", Project: "B4T", ProjectID: "proj-1"},
		Item:    Item{ID: "epic-1", Name: "Toy epic", Description: "Build a toy.", SequenceID: 7},
		ModelID: "qwen3.8-27b",
		Effort:  "high",
	}
}

func toyTasks() TaskList {
	return TaskList{Epic: "epic-1", Tasks: []Task{
		{ID: "t1", Title: "Base", Description: "Build the base", AcceptanceCheck: "go test ./...", Size: "S", ExecutorModel: "qwen3.8-27b"},
		{ID: "t2", Title: "Top", Description: "Build the top", Files: []string{"a.go"}, Dependencies: []string{"t1"}, AcceptanceCheck: "go test ./...", Size: "M", ExecutorModel: "qwen3.8-27b"},
		{ID: "t3", Title: "Glue", Description: "Glue it together", Dependencies: []string{"t1", "t2"}, AcceptanceCheck: "make check", Size: "S", ExecutorModel: "qwen3.8-27b"},
	}}
}

func toyResult() *Result {
	return &Result{Plan: "# Plan\n\nSome plan.", Tasks: toyTasks(), Model: registry.Model{ID: "qwen3.8-27b"}}
}

// --- fake board ----------------------------------------------------------------

// fakeBoard is an in-memory stand-in for this Plane build's two API
// surfaces, covering exactly what the deliverer uses: v1 states, labels,
// work items, relations and comments (paginated lists, PATCH-replaces-the-
// whole-label-list, the "issues" relation field) plus the session API for
// pages (CSRF, sign-in, bare-array list).
type fakeBoard struct {
	mu       sync.Mutex
	seq      int
	states   map[string]*fpState
	labels   map[string]*fpLabel
	items    map[string]*fpItem
	blocked  map[string][]string // item id -> blocked-by item ids
	pages    []*fpPage
	comments map[string][]string // item id -> comment html
}

type fpState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Group    string  `json:"group"`
	Sequence float64 `json:"sequence"`
	Color    string  `json:"color"`
}

type fpLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type fpItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html"`
	State           string   `json:"state"`
	Labels          []string `json:"labels"`
	Parent          string   `json:"parent"`
	SequenceID      int      `json:"sequence_id"`
	Priority        string   `json:"priority"`
}

type fpPage struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	DescriptionHTML string `json:"description_html"`
}

func newFakeBoard() *fakeBoard {
	return &fakeBoard{
		states: map[string]*fpState{
			"st-backlog":     {ID: "st-backlog", Name: "Backlog", Group: "backlog", Sequence: 65536, Color: "#94A3B8"},
			"st-plan-review": {ID: "st-plan-review", Name: "Plan review", Group: "unstarted", Sequence: 131072, Color: "#FBBF24"},
		},
		labels: map[string]*fpLabel{},
		items: map[string]*fpItem{
			"epic-1": {ID: "epic-1", Name: "Toy epic", DescriptionHTML: "Build a toy.", State: "st-backlog", Priority: "none"},
		},
		blocked:  map[string][]string{},
		comments: map[string][]string{},
	}
}

const fpV1Root = "/api/v1/workspaces/testws/"

func (f *fakeBoard) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/auth/get-csrf-token/":
			fpJSON(w, 200, map[string]string{"csrf_token": "csrf-test"})
		case r.URL.Path == "/auth/sign-in/":
			w.Header().Add("Set-Cookie", "sessionid=fake-session; Path=/")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, fpV1Root):
			f.routeV1(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/workspaces/"):
			f.routePages(w, r)
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func (f *fakeBoard) routeV1(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, fpV1Root), "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] != "projects" {
		http.Error(w, "bad path: "+r.URL.Path, http.StatusNotFound)
		return
	}
	pid := parts[1]
	switch {
	case len(parts) == 3 && parts[2] == "states" && r.Method == "GET":
		fpJSON(w, 200, map[string]any{"results": f.stateList()})
	case len(parts) == 3 && parts[2] == "labels" && r.Method == "GET":
		fpJSON(w, 200, map[string]any{"results": f.labelList()})
	case len(parts) == 3 && parts[2] == "labels" && r.Method == "POST":
		var in struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.seq++
		l := &fpLabel{ID: fmt.Sprintf("lb-%d", f.seq), Name: in.Name, Color: in.Color}
		f.labels[l.ID] = l
		fpJSON(w, 201, l)
	case len(parts) == 3 && parts[2] == "work-items" && r.Method == "GET":
		fpJSON(w, 200, map[string]any{"results": f.itemList(pid)})
	case len(parts) == 3 && parts[2] == "work-items" && r.Method == "POST":
		var in struct {
			Name   string   `json:"name"`
			Desc   string   `json:"description_html"`
			Parent string   `json:"parent"`
			State  string   `json:"state"`
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.seq++
		it := &fpItem{ID: fmt.Sprintf("wi-%d", f.seq), Name: in.Name, DescriptionHTML: in.Desc,
			Parent: in.Parent, State: in.State, Labels: in.Labels, Priority: "none"}
		f.items[it.ID] = it
		fpJSON(w, 201, it)
	case len(parts) == 4 && parts[2] == "work-items" && r.Method == "GET":
		it, ok := f.items[parts[3]]
		if !ok {
			http.Error(w, "no such item", http.StatusNotFound)
			return
		}
		fpJSON(w, 200, it)
	case len(parts) == 4 && parts[2] == "work-items" && r.Method == "PATCH":
		it, ok := f.items[parts[3]]
		if !ok {
			http.Error(w, "no such item", http.StatusNotFound)
			return
		}
		var in struct {
			State           *string  `json:"state"`
			Labels          []string `json:"labels"`
			DescriptionHTML *string  `json:"description_html"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if in.State != nil {
			it.State = *in.State
		}
		if in.Labels != nil { // this build's PATCH replaces the whole list
			it.Labels = in.Labels
		}
		if in.DescriptionHTML != nil {
			it.DescriptionHTML = *in.DescriptionHTML
		}
		fpJSON(w, 200, it)
	case len(parts) == 5 && parts[2] == "work-items" && parts[4] == "relations" && r.Method == "POST":
		var in struct {
			RelationType string   `json:"relation_type"`
			Issues       []string `json:"issues"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RelationType != "blocked_by" || in.Issues == nil {
			http.Error(w, "bad relation payload", http.StatusBadRequest)
			return
		}
		f.blocked[parts[3]] = append(f.blocked[parts[3]], in.Issues...)
		fpJSON(w, 201, map[string]any{"count": len(in.Issues)})
	case len(parts) == 5 && parts[2] == "work-items" && parts[4] == "relations" && r.Method == "GET":
		refs := []map[string]string{}
		for _, id := range f.blocked[parts[3]] {
			refs = append(refs, map[string]string{"project_id": pid, "issue_id": id})
		}
		fpJSON(w, 200, map[string]any{"blocked_by": refs})
	case len(parts) == 5 && parts[2] == "work-items" && parts[4] == "comments" && r.Method == "POST":
		var in struct {
			HTML string `json:"comment_html"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.comments[parts[3]] = append(f.comments[parts[3]], in.HTML)
		f.seq++
		fpJSON(w, 201, map[string]string{"id": fmt.Sprintf("cm-%d", f.seq), "comment_html": in.HTML})
	default:
		http.Error(w, "unhandled: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeBoard) routePages(w http.ResponseWriter, r *http.Request) {
	const root = "/api/workspaces/testws/projects/proj-1/pages/"
	if !strings.HasPrefix(r.URL.Path, root) {
		http.Error(w, "unhandled: "+r.URL.Path, http.StatusNotFound)
		return
	}
	// A single page (the detail endpoint): the only place the body comes back.
	if id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, root), "/"); id != "" {
		for _, p := range f.pages {
			if p.ID == id {
				fpJSON(w, 200, p)
				return
			}
		}
		http.Error(w, "no such page", http.StatusNotFound)
		return
	}
	switch r.Method {
	case "GET":
		// Like the real build, the list omits the page body.
		sums := []map[string]string{}
		for _, p := range f.pages {
			sums = append(sums, map[string]string{"id": p.ID, "name": p.Name})
		}
		fpJSON(w, 200, sums) // session lists are bare arrays
	case "POST":
		var in struct {
			Name string `json:"name"`
			HTML string `json:"description_html"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.seq++
		pg := &fpPage{ID: fmt.Sprintf("pg-%d", f.seq), Name: in.Name, DescriptionHTML: in.HTML}
		f.pages = append(f.pages, pg)
		fpJSON(w, 201, pg)
	default:
		http.Error(w, "unhandled method", http.StatusMethodNotAllowed)
	}
}

func (f *fakeBoard) stateList() []*fpState {
	out := make([]*fpState, 0, len(f.states))
	for _, s := range f.states {
		out = append(out, s)
	}
	return out
}

func (f *fakeBoard) labelList() []*fpLabel {
	out := make([]*fpLabel, 0, len(f.labels))
	for _, l := range f.labels {
		out = append(out, l)
	}
	return out
}

func (f *fakeBoard) itemList(projectID string) []*fpItem {
	out := []*fpItem{}
	for _, it := range f.items {
		out = append(out, it)
	}
	return out
}

func fpJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

// --- tests ------------------------------------------------------------------------

func newDelivererTest(t *testing.T) (*Deliverer, *fakeBoard, *httptest.Server) {
	t.Helper()
	fb := newFakeBoard()
	srv := httptest.NewServer(fb.handler())
	t.Cleanup(srv.Close)
	c, err := plane.New(plane.Config{
		BaseURL:   srv.URL,
		APIKey:    "test-key",
		Workspace: "testws",
		Email:     "bot@example.com",
		Password:  "pw",
	})
	if err != nil {
		t.Fatalf("plane.New: %v", err)
	}
	return &Deliverer{Plane: c, Registry: deliverRegistry()}, fb, srv
}

func modelLabelID(fb *fakeBoard) string {
	for _, l := range fb.labels {
		if l.Name == placement.ModelLabel("qwen3.8-27b") {
			return l.ID
		}
	}
	return ""
}

func subItemsOf(fb *fakeBoard, epicID string) map[string]*fpItem {
	out := map[string]*fpItem{}
	for _, it := range fb.items {
		if it.Parent != epicID {
			continue
		}
		if tid, ok := markerValue(it.DescriptionHTML, planTaskMarker); ok {
			out[tid] = it
		} else {
			out[it.ID] = it // unmarked card; still count it
		}
	}
	return out
}

func TestDeliverFull(t *testing.T) {
	d, fb, srv := newDelivererTest(t)
	del, err := d.Deliver(context.Background(), toyJob(), toyResult())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// The plan page exists, named after the epic, with the plan in it.
	if len(fb.pages) != 1 || fb.pages[0].Name != "Plan: Toy epic" {
		t.Fatalf("pages = %+v, want one page named 'Plan: Toy epic'", fb.pages)
	}
	if !strings.Contains(fb.pages[0].DescriptionHTML, "Some plan.") {
		t.Errorf("page body %q does not contain the plan text", fb.pages[0].DescriptionHTML)
	}
	if fb.pages[0].ID != del.Page.ID {
		t.Errorf("Delivery.Page = %s, board page = %s", del.Page.ID, fb.pages[0].ID)
	}

	// The model: label was created in the model colour.
	labID := modelLabelID(fb)
	if labID == "" {
		t.Fatalf("labels = %+v, want %s", fb.labels, placement.ModelLabel("qwen3.8-27b"))
	}
	for id, l := range fb.labels {
		if l.Color != placement.ModelColor {
			t.Errorf("label %s colour = %s, want %s", id, l.Color, placement.ModelColor)
		}
	}

	// Three sub-items in Plan review, each with the model label.
	subs := subItemsOf(fb, "epic-1")
	if len(subs) != 3 {
		t.Fatalf("sub-items = %d, want 3", len(subs))
	}
	for tid, s := range subs {
		if s.State != "st-plan-review" {
			t.Errorf("task %s: state = %s, want Plan review", tid, s.State)
		}
		if len(s.Labels) != 1 || s.Labels[0] != labID {
			t.Errorf("task %s: labels = %v, want [%s]", tid, s.Labels, labID)
		}
		if !strings.HasPrefix(s.DescriptionHTML, "<p>"+planTaskMarker+tid+"</p>") {
			t.Errorf("task %s: description does not start with the marker line: %q", tid, s.DescriptionHTML)
		}
	}

	// Blocked-by relations: t2 <- t1, t3 <- t1, t2.
	if got := fb.blocked[subs["t2"].ID]; !fpContainsAll(got, subs["t1"].ID) || len(got) != 1 {
		t.Errorf("t2 blocked_by = %v, want [%s]", got, subs["t1"].ID)
	}
	if got := fb.blocked[subs["t3"].ID]; !fpContainsAll(got, subs["t1"].ID, subs["t2"].ID) || len(got) != 2 {
		t.Errorf("t3 blocked_by = %v, want [%s %s]", got, subs["t1"].ID, subs["t2"].ID)
	}
	if len(fb.blocked[subs["t1"].ID]) != 0 {
		t.Errorf("t1 blocked_by = %v, want none", fb.blocked[subs["t1"].ID])
	}

	// The epic: description links the page exactly once, summary comment,
	// and state moved to Plan review last.
	epic := fb.items["epic-1"]
	wantURL := srv.URL + "/testws/projects/proj-1/pages/" + fb.pages[0].ID
	if n := strings.Count(epic.DescriptionHTML, planPageMarker); n != 1 {
		t.Errorf("epic description has %d plan-page lines, want 1: %q", n, epic.DescriptionHTML)
	}
	if !strings.Contains(epic.DescriptionHTML, planPageMarker+wantURL) {
		t.Errorf("epic description %q does not contain the plan-page link %s", epic.DescriptionHTML, wantURL)
	}
	if !strings.Contains(epic.DescriptionHTML, "Build a toy.") {
		t.Errorf("epic description lost its original text: %q", epic.DescriptionHTML)
	}
	if len(fb.comments["epic-1"]) != 1 || !strings.Contains(fb.comments["epic-1"][0], "Plan ready") {
		t.Errorf("epic comments = %v, want one 'Plan ready' summary", fb.comments["epic-1"])
	}
	if !strings.Contains(fb.comments["epic-1"][0], wantURL) {
		t.Errorf("summary comment %q does not carry the page link", fb.comments["epic-1"][0])
	}
	if epic.State != "st-plan-review" {
		t.Errorf("epic state = %s, want Plan review", epic.State)
	}
}

func TestDeliverRejectsUnknownExecutor(t *testing.T) {
	d, fb, _ := newDelivererTest(t)
	res := toyResult()
	res.Tasks.Tasks[0].ExecutorModel = "ghost-model"
	_, err := d.Deliver(context.Background(), toyJob(), res)
	if err == nil || !strings.Contains(err.Error(), "ghost-model") {
		t.Fatalf("Deliver err = %v, want an unknown-executor error naming ghost-model", err)
	}
	if fb.items["epic-1"].State != "st-backlog" {
		t.Errorf("epic state = %s, want Backlog (no move on failure)", fb.items["epic-1"].State)
	}
	if len(subItemsOf(fb, "epic-1")) != 0 {
		t.Errorf("sub-items created despite failed delivery: %v", subItemsOf(fb, "epic-1"))
	}
	if len(fb.pages) != 0 {
		t.Errorf("pages created despite failed delivery: %v", fb.pages)
	}
}

func TestDeliverConvergesOnRedelivery(t *testing.T) {
	d, fb, _ := newDelivererTest(t)
	job, res := toyJob(), toyResult()
	if _, err := d.Deliver(context.Background(), job, res); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	if _, err := d.Deliver(context.Background(), job, res); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}

	if got := len(subItemsOf(fb, "epic-1")); got != 3 {
		t.Errorf("sub-items after re-delivery = %d, want 3 (no duplicates)", got)
	}
	if got := len(fb.pages); got != 1 {
		t.Errorf("pages after re-delivery = %d, want 1", got)
	}
	epic := fb.items["epic-1"]
	if n := strings.Count(epic.DescriptionHTML, planPageMarker); n != 1 {
		t.Errorf("plan-page lines after re-delivery = %d, want 1: %q", n, epic.DescriptionHTML)
	}
	if epic.State != "st-plan-review" {
		t.Errorf("epic state after re-delivery = %s, want Plan review", epic.State)
	}
	// Labels and relations survived the re-delivery.
	labID := modelLabelID(fb)
	for _, s := range subItemsOf(fb, "epic-1") {
		if len(s.Labels) != 1 || s.Labels[0] != labID {
			t.Errorf("sub-item %s labels after re-delivery = %v, want [%s]", s.ID, s.Labels, labID)
		}
	}
}

// TestDeliverConvergesAPartialDelivery simulates a retry after a delivery
// that failed after creating two of three sub-items: the re-run finishes
// the plan without duplicating the cards.
func TestDeliverConvergesAPartialDelivery(t *testing.T) {
	d, fb, _ := newDelivererTest(t)
	job, res := toyJob(), toyResult()

	// Pre-seed what a partially failed first delivery left behind: the
	// two sub-items (still in Backlog, no labels yet) but no page and the
	// epic still in Backlog.
	state, err := d.Plane.StateByName(context.Background(), "proj-1", "Backlog")
	if err != nil || state == nil {
		t.Fatalf("no Backlog state: %v", err)
	}
	for _, tid := range []string{"t1", "t2"} {
		tk := res.Tasks.Tasks[0]
		if tid == "t2" {
			tk = res.Tasks.Tasks[1]
		}
		if _, err := d.Plane.CreateWorkItem(context.Background(), "proj-1", plane.WorkItemCreate{
			Name:            tk.Title,
			DescriptionHTML: taskDescriptionHTML(tk),
			Parent:          "epic-1",
			State:           state.ID,
		}); err != nil {
			t.Fatalf("seed %s: %v", tid, err)
		}
	}

	if _, err := d.Deliver(context.Background(), job, res); err != nil {
		t.Fatalf("Deliver after partial: %v", err)
	}
	subs := subItemsOf(fb, "epic-1")
	if len(subs) != 3 {
		t.Fatalf("sub-items = %d, want 3 (t1/t2 reused, t3 new)", len(subs))
	}
	for tid, s := range subs {
		if s.State != "st-plan-review" {
			t.Errorf("task %s state = %s, want Plan review", tid, s.State)
		}
	}
	if len(fb.pages) != 1 {
		t.Errorf("pages = %d, want 1", len(fb.pages))
	}
}

func fpContainsAll(have []string, want ...string) bool {
	for _, w := range want {
		ok := false
		for _, h := range have {
			if h == w {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// --- helper units -------------------------------------------------------------------

func TestMarkerValue(t *testing.T) {
	cases := []struct {
		s      string
		marker string
		want   string
		ok     bool
	}{
		{"<p>plan-task: t1</p>\n<p>body</p>", planTaskMarker, "t1", true},
		{"plan-task: a-b-9", planTaskMarker, "a-b-9", true},
		{"no marker here", planTaskMarker, "", false},
		{"", planTaskMarker, "", false},
	}
	for _, c := range cases {
		if got, ok := markerValue(c.s, c.marker); got != c.want || ok != c.ok {
			t.Errorf("markerValue(%q) = (%q, %v), want (%q, %v)", c.s, got, ok, c.want, c.ok)
		}
	}
}

func TestDropMarkerParagraph(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no marker", "<p>hello</p>", "<p>hello</p>"},
		{"marker line dropped", "<p>hello</p>\n<p>plan-page: http://x</p>", "<p>hello</p>"},
		{"marker line is the only line", "<p>plan-page: http://x</p>", ""},
		{"malformed marker kept", "plan-page: http://x", "plan-page: http://x"},
	}
	for _, c := range cases {
		if got := dropMarkerParagraph(c.in, planPageMarker); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestTaskDescriptionHTML(t *testing.T) {
	tk := Task{
		ID: "t1", Title: "T",
		Description:     "A <b>bold</b> plan",
		Dependencies:    []string{"x"},
		AcceptanceCheck: "go test ./...",
		Size:            "M",
		ExecutorModel:   "qwen3.8-27b",
	}
	got := taskDescriptionHTML(tk)
	if !strings.HasPrefix(got, "<p>"+planTaskMarker+"t1</p>") {
		t.Errorf("description must start with the marker line, got %q", got)
	}
	if !strings.Contains(got, "A &lt;b&gt;bold&lt;/b&gt; plan") {
		t.Errorf("description not HTML-escaped: %q", got)
	}
	if !strings.Contains(got, "model:qwen3.8-27b") {
		t.Errorf("description should name the executor label: %q", got)
	}
}

func TestPlanPageHTML(t *testing.T) {
	got := planPageHTML("# Title\n\n`code` & text")
	want := "<pre># Title\n\n`code` &amp; text</pre>"
	if got != want {
		t.Errorf("planPageHTML = %q, want %q", got, want)
	}
}
