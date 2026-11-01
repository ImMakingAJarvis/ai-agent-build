package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakePlane is an in-memory stand-in for this Plane build's two API
// surfaces, including its quirks: v1 lists come back paginated, session
// lists as bare arrays, work-item PATCH replaces the whole label list,
// and the relation payload field is "issues".
type fakePlane struct {
	mu sync.Mutex

	projects map[string]map[string]string
	states   map[string][]map[string]any
	labels   map[string][]map[string]string
	items    map[string]map[string]any
	// relations[itemID] = list of {type, otherID}
	relations map[string][]map[string]string
	comments  []string
	pages     []map[string]string
	// webhooks[id] = {id, url, is_active, secret_key, issue, issue_comment, ...}
	webhooks map[string]map[string]any

	// server-side observation of what the client sent
	lastLabelPatch []string
	relationsPost  map[string]any
	commentFields  []string

	// session observations
	signinEmail string
	signinCSRF  string
	pageCSRF    string
	pageCookie  string
	sessionOK   bool
}

func newFakePlane() *fakePlane {
	return &fakePlane{
		projects:  map[string]map[string]string{},
		states:    map[string][]map[string]any{},
		labels:    map[string][]map[string]string{},
		items:     map[string]map[string]any{},
		relations: map[string][]map[string]string{},
		pages:     []map[string]string{},
		webhooks:  map[string]map[string]any{},
	}
}

func (f *fakePlane) handler() http.Handler {
	mux := http.NewServeMux()
	// --- auth (session API) ---
	mux.HandleFunc("GET /auth/get-csrf-token/", func(w http.ResponseWriter, r *http.Request) {
		csrf := "csrf-anon"
		if f.sessionCookie(r) != "" {
			csrf = "csrf-post-login" // the token moves into the session
		}
		json.NewEncoder(w).Encode(map[string]string{"csrf_token": csrf})
	})
	mux.HandleFunc("POST /auth/sign-in/", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.signinCSRF = r.Header.Get("X-CSRFToken")
		f.mu.Unlock()
		f.mu.Lock()
		f.signinEmail = r.PostForm.Get("email")
		ok := f.signinEmail == "bot@example.com" && r.PostForm.Get("password") == "pw" &&
			f.signinCSRF == "csrf-anon" &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
		if ok {
			f.sessionOK = true
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "bad credentials", http.StatusForbidden)
			return
		}
		// Like the real server: Set-Cookie with Path / and a 302 to the
		// site root (a real deploy redirects to its SITE_URL, which may
		// be unreachable from the client — the client must not follow).
		http.SetCookie(w, &http.Cookie{Name: "session-id", Value: "fakesess", Path: "/"})
		w.Header().Set("Location", "http://unreachable.example/")
		w.WriteHeader(http.StatusFound)
	})

	// --- v1 external API ---
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "testkey" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"detail": "credentials were not provided"})
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/")
		ws, rem, ok := strings.Cut(p, "/")
		if !ok || ws != "testws" {
			http.Error(w, "no such workspace", http.StatusNotFound)
			return
		}
		f.routeV1(w, r, rem)
	})
	mux.Handle("/api/v1/", v1)

	// --- session API: pages (cookie + CSRF required, bare-array lists) ---
	mux.HandleFunc("POST /api/workspaces/testws/projects/{pid}/pages/", func(w http.ResponseWriter, r *http.Request) {
		if f.sessionCookie(r) != "fakesess" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"detail": "Authentication credentials were not provided."})
			return
		}
		f.mu.Lock()
		f.pageCSRF = r.Header.Get("X-CSRFToken")
		f.pageCookie = f.sessionCookie(r)
		f.mu.Unlock()
		if f.pageCSRF != "csrf-post-login" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("CSRF Failed"))
			return
		}
		var pg Page
		json.NewDecoder(r.Body).Decode(&pg)
		pg.ID = fmt.Sprintf("page-%d", len(f.pages)+1)
		pg.CreatedAt = "2026-10-31T00:00:00Z"
		f.mu.Lock()
		f.pages = append(f.pages, map[string]string{
			"id": pg.ID, "name": pg.Name, "content_html": pg.ContentHTML, "created_at": pg.CreatedAt,
		})
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(pg)
	})
	mux.HandleFunc("GET /api/workspaces/testws/projects/{pid}/pages/", func(w http.ResponseWriter, r *http.Request) {
		if f.sessionCookie(r) != "fakesess" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"detail": "Authentication credentials were not provided."})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.pages) // bare array, like session endpoints
	})

	// --- session API: workspace webhooks (cookie + CSRF required) ---
	// Like the real build, the webhook endpoints are session-API only and
	// admin-only; the v1 external API has no webhook routes.
	webhook := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.sessionCookie(r) != "fakesess" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"detail": "Authentication credentials were not provided."})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRFToken") != "csrf-post-login" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("CSRF Failed"))
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/workspaces/testws/webhooks")
		f.routeWebhook(w, r, rest)
	})
	mux.Handle("/api/workspaces/testws/webhooks/", webhook)

	return mux
}

// sessionCookie reads the session cookie (either name) off the request.
func (f *fakePlane) sessionCookie(r *http.Request) string {
	if c, err := r.Cookie("session-id"); err == nil {
		return c.Value
	}
	if c, err := r.Cookie("sessionid"); err == nil {
		return c.Value
	}
	return ""
}

// routeV1 dispatches the workspace-scoped v1 routes (rest without the
// workspace prefix, e.g. "projects/" or "projects/<id>/states/").
func (f *fakePlane) routeV1(w http.ResponseWriter, r *http.Request, rest string) {
	rest = "/" + rest
	// /members/ — the workspace member list (one bot member, like the
	// real jarvis workspace before the admin invite is accepted).
	if (rest == "/members" || rest == "/members/") && r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"id": "user-bot", "email": "bot@example.com", "role": 20},
		}, "count": 1})
		return
	}
	// /projects/ and /projects/{id}/...
	if rest == "/projects" || rest == "/projects/" {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			out := make([]map[string]string, 0, len(f.projects))
			for _, p := range f.projects {
				out = append(out, p)
			}
			f.mu.Unlock()
			sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
			json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Name       string `json:"name"`
				Identifier string `json:"identifier"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			p := map[string]string{"id": fmt.Sprintf("proj-%d", len(f.projects)+1), "name": body.Name, "identifier": body.Identifier}
			f.mu.Lock()
			f.projects[p["id"]] = p
			// fresh projects get the five default states, like the real board
			names := []string{"Backlog", "Todo", "In Progress", "Done", "Cancelled"}
			var sts []map[string]any
			for i, n := range names {
				sts = append(sts, map[string]any{
					"id": fmt.Sprintf("st-%d", i+1), "name": n,
					"group": "unstarted", "sequence": float64(i+1) * 1000,
				})
			}
			f.states[p["id"]] = sts
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(p)
			return
		}
	}
	pid, rest2, ok := strings.Cut(strings.TrimPrefix(rest, "/projects/"), "/")
	if ok {
		switch {
		case strings.HasPrefix(rest2, "states") && (rest2 == "states" || rest2 == "states/"):
			if r.Method == http.MethodGet {
				f.mu.Lock()
				out := append([]map[string]any(nil), f.states[pid]...)
				f.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
				return
			}
			if r.Method == http.MethodPost {
				var body struct {
					Name     string  `json:"name"`
					Group    string  `json:"group"`
					Color    string  `json:"color"`
					Sequence float64 `json:"sequence"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				st := map[string]any{
					"id": fmt.Sprintf("st-%d", len(f.states[pid])+1), "name": body.Name,
					"group": body.Group, "sequence": body.Sequence,
				}
				if body.Color != "" {
					st["color"] = body.Color
				}
				f.mu.Lock()
				f.states[pid] = append(f.states[pid], st)
				f.mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(st)
				return
			}
		case strings.HasPrefix(rest2, "labels/") && len(rest2) > len("labels/"):
			// DELETE /projects/<id>/labels/<label-id>/
			if r.Method != http.MethodDelete {
				http.Error(w, "unrouted v1 path "+rest, http.StatusNotFound)
				return
			}
			lid := strings.TrimSuffix(strings.TrimPrefix(rest2, "labels/"), "/")
			f.mu.Lock()
			for i, l := range f.labels[pid] {
				if l["id"] == lid {
					f.labels[pid] = append(f.labels[pid][:i], f.labels[pid][i+1:]...)
					break
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		case strings.HasPrefix(rest2, "labels") && (rest2 == "labels" || rest2 == "labels/"):
			if r.Method == http.MethodGet {
				f.mu.Lock()
				out := append([]map[string]string(nil), f.labels[pid]...)
				f.mu.Unlock()
				sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
				json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
				return
			}
			if r.Method == http.MethodPost {
				var body struct {
					Name  string `json:"name"`
					Color string `json:"color"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				f.mu.Lock()
				l := map[string]string{"id": fmt.Sprintf("lbl-%d", len(f.labels[pid])+1), "name": body.Name, "color": body.Color}
				f.labels[pid] = append(f.labels[pid], l)
				f.mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(l)
				return
			}
		case strings.HasPrefix(rest2, "work-items") || rest2 == "work-items" || rest2 == "work-items/":
			f.routeWorkItem(w, r, pid, rest2)
			return
		case rest2 == "" || rest2 == "/":
			// GET /projects/<id>/ — single project by UUID
			if r.Method == http.MethodGet {
				f.mu.Lock()
				p, ok := f.projects[pid]
				f.mu.Unlock()
				if !ok {
					http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
					return
				}
				json.NewEncoder(w).Encode(p)
				return
			}
		}
	}
	http.Error(w, fmt.Sprintf("unrouted v1 path %s", rest), http.StatusNotFound)
}

// routeWebhook handles the workspace webhook routes: list, create,
// retrieve, delete, and secret regeneration.
func (f *fakePlane) routeWebhook(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/")
	id, _, _ := strings.Cut(rest, "/")
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			out := make([]map[string]any, 0, len(f.webhooks))
			for _, wh := range f.webhooks {
				// the real API never returns the secret on list
				cp := map[string]any{}
				for k, v := range wh {
					if k != "secret_key" {
						cp[k] = v
					}
				}
				out = append(out, cp)
			}
			f.mu.Unlock()
			sort.Slice(out, func(i, j int) bool {
				return out[i]["id"].(string) < out[j]["id"].(string)
			})
			json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			url, _ := body["url"].(string)
			if url == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"url": []string{"This field is required."}})
				return
			}
			f.mu.Lock()
			for _, wh := range f.webhooks {
				if wh["url"] == url {
					f.mu.Unlock()
					w.WriteHeader(http.StatusConflict)
					json.NewEncoder(w).Encode(map[string]string{"error": "URL already exists for the workspace"})
					return
				}
			}
			wh := map[string]any{
				"id": fmt.Sprintf("wh-%d", len(f.webhooks)+1), "url": url, "is_active": true,
				"secret_key": fmt.Sprintf("plane_wh-%d", len(f.webhooks)+1),
				"issue":      body["issue"] == true, "issue_comment": body["issue_comment"] == true,
			}
			f.webhooks[wh["id"].(string)] = wh
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(wh) // secret revealed on create
		}
		return
	}
	switch {
	case rest == id:
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			wh, ok := f.webhooks[id]
			f.mu.Unlock()
			if !ok {
				http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
				return
			}
			cp := map[string]any{}
			for k, v := range wh {
				if k != "secret_key" {
					cp[k] = v
				}
			}
			json.NewEncoder(w).Encode(cp)
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.webhooks, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	case strings.TrimSuffix(rest, "/regenerate") == id && strings.HasSuffix(rest, "/regenerate"):
		f.mu.Lock()
		wh, ok := f.webhooks[id]
		if ok {
			wh["secret_key"] = fmt.Sprintf("plane_wh-%d", len(f.webhooks)+100)
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(wh) // secret revealed on regenerate
	}
}

// routeWorkItem handles /work-items and /work-items/{id}[/{relations,comments}].
func (f *fakePlane) routeWorkItem(w http.ResponseWriter, r *http.Request, pid, rest2 string) {
	sub := strings.Trim(strings.TrimPrefix(rest2, "work-items"), "/")
	if sub == "" {
		if r.Method == http.MethodGet {
			// paginated list (limit/page), sorted by sequence_id
			f.mu.Lock()
			all := make([]map[string]any, 0, len(f.items))
			for _, it := range f.items {
				all = append(all, it)
			}
			f.mu.Unlock()
			sort.Slice(all, func(i, j int) bool {
				si, _ := all[i]["sequence_id"].(int)
				sj, _ := all[j]["sequence_id"].(int)
				return si < sj
			})
			q := r.URL.Query()
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit <= 0 {
				limit = 25
			}
			page, _ := strconv.Atoi(q.Get("page"))
			if page <= 0 {
				page = 1
			}
			start := (page - 1) * limit
			if start > len(all) {
				start = len(all)
			}
			end := start + limit
			if end > len(all) {
				end = len(all)
			}
			json.NewEncoder(w).Encode(all[start:end])
			return
		}
		if r.Method == http.MethodPost {
			var body WorkItemCreate
			json.NewDecoder(r.Body).Decode(&body)
			id := fmt.Sprintf("wi-%d", len(f.items)+1)
			item := map[string]any{
				"id": id, "name": body.Name, "description_html": body.DescriptionHTML,
				"state": "st-1", "labels": []string{}, "parent": body.Parent,
				"sequence_id": len(f.items) + 1, "created_at": "2026-10-31T00:00:00Z",
			}
			if body.State != "" {
				item["state"] = body.State
			}
			if body.Labels != nil {
				item["labels"] = body.Labels
			}
			f.mu.Lock()
			f.items[id] = item
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(item)
			return
		}
	}
	id, tail, _ := strings.Cut(sub, "/")
	f.mu.Lock()
	item, exists := f.items[id]
	f.mu.Unlock()
	if !exists {
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
		return
	}
	switch tail {
	case "", "/":
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(item)
		case http.MethodPatch:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			if s, ok := body["state"].(string); ok {
				item["state"] = s
			}
			// this build replaces the whole label list on PATCH
			if ls, ok := body["labels"].([]any); ok {
				out := make([]string, 0, len(ls))
				for _, l := range ls {
					out = append(out, l.(string))
				}
				item["labels"] = out
				f.lastLabelPatch = out
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(item)
		}
	case "relations":
		switch r.Method {
		case http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			issues, _ := body["issues"].([]any)
			if body["relation_type"] != "blocked_by" || len(issues) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"issues": []string{"This field is required."}})
				return
			}
			f.mu.Lock()
			f.relationsPost = body
			for _, is := range issues {
				f.relations[id] = append(f.relations[id], map[string]string{"type": "blocked_by", "other": is.(string)})
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`[]`))
		case http.MethodGet:
			f.mu.Lock()
			out := map[string][]map[string]string{"blocking": {}, "blocked_by": {}, "duplicate": {}, "relates_to": {}, "start_after": {}, "start_before": {}, "finish_after": {}, "finish_before": {}}
			for _, rel := range f.relations[id] {
				out[rel["type"]] = append(out[rel["type"]], map[string]string{"project_id": pid, "issue_id": rel["other"]})
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(out)
		}
	case "comments":
		switch r.Method {
		case http.MethodGet:
			// paginated list, like the v1 list endpoints
			f.mu.Lock()
			out := make([]map[string]string, 0, len(f.comments))
			for i, html := range f.comments {
				out = append(out, map[string]string{"id": fmt.Sprintf("cm-%d", i+1), "comment_html": html, "created_at": "2026-10-31T00:00:00Z"})
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
		case http.MethodPost:
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			f.mu.Lock()
			f.commentFields = make([]string, 0, len(raw))
			for k := range raw {
				f.commentFields = append(f.commentFields, k)
			}
			html, _ := raw["comment_html"].(string)
			f.comments = append(f.comments, html)
			f.mu.Unlock()
			if html == "" {
				http.Error(w, `{"detail":"comment_html is required"}`, http.StatusBadRequest)
				return
			}
			cm := map[string]string{"id": fmt.Sprintf("cm-%d", len(f.comments)), "comment_html": html, "created_at": "2026-10-31T00:00:00Z"}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(cm)
		}
	}
}

// newTestClient points a Client at the fake and returns both.
func newTestClient(t *testing.T) (*Client, *fakePlane, *httptest.Server) {
	t.Helper()
	fake := newFakePlane()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "testkey", Workspace: "testws", Email: "bot@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fake, srv
}

func mustProject(t *testing.T, c *Client) string {
	t.Helper()
	p, err := c.CreateProject(context.Background(), "t", "TST")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return p.ID
}

func TestCreateWorkItemAndSubItem(t *testing.T) {
	c, _, _ := newTestClient(t)
	pid := mustProject(t, c)

	parent, err := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "parent"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	child, err := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "child", Parent: parent.ID})
	if err != nil {
		t.Fatalf("CreateWorkItem child: %v", err)
	}
	if child.Parent != parent.ID {
		t.Errorf("child.Parent = %q, want %q", child.Parent, parent.ID)
	}
	got, err := c.GetWorkItem(context.Background(), pid, child.ID)
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if got.Name != "child" || got.Parent != parent.ID {
		t.Errorf("GetWorkItem = %+v, want name=child parent=%s", got, parent.ID)
	}
}

func TestSetState(t *testing.T) {
	c, _, _ := newTestClient(t)
	pid := mustProject(t, c)
	wi, _ := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "x"})

	done, err := c.StateByName(context.Background(), pid, "Done")
	if err != nil {
		t.Fatalf("StateByName: %v", err)
	}
	got, err := c.SetState(context.Background(), pid, wi.ID, done.ID)
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if got.State != done.ID {
		t.Errorf("state = %q, want %q", got.State, done.ID)
	}
}

func TestAddRemoveLabels(t *testing.T) {
	c, fake, _ := newTestClient(t)
	pid := mustProject(t, c)
	wi, _ := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "x"})

	a, err := c.CreateLabel(context.Background(), pid, "alpha", "#111111")
	if err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	b, err := c.CreateLabel(context.Background(), pid, "beta", "#222222")
	if err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}

	got, err := c.AddLabels(context.Background(), pid, wi.ID, a.ID)
	if err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != a.ID {
		t.Errorf("labels after add(a) = %v, want [%s]", got.Labels, a.ID)
	}

	// The server replaces the whole list, so the second PATCH must carry
	// both labels — proof the client read the item first.
	got, err = c.AddLabels(context.Background(), pid, wi.ID, b.ID)
	if err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	fake.mu.Lock()
	last := append([]string(nil), fake.lastLabelPatch...)
	fake.mu.Unlock()
	if len(last) != 2 || !containsAll(last, a.ID, b.ID) {
		t.Errorf("server last label list = %v, want both %v and %v", last, a.ID, b.ID)
	}
	if len(got.Labels) != 2 {
		t.Errorf("labels after add(b) = %v, want 2", got.Labels)
	}

	got, err = c.RemoveLabels(context.Background(), pid, wi.ID, a.ID)
	if err != nil {
		t.Fatalf("RemoveLabels: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != b.ID {
		t.Errorf("labels after remove(a) = %v, want [%s]", got.Labels, b.ID)
	}
}

func TestDeleteLabel(t *testing.T) {
	c, _, _ := newTestClient(t)
	pid := mustProject(t, c)
	keep, err := c.CreateLabel(context.Background(), pid, "keep", "#111111")
	if err != nil {
		t.Fatalf("CreateLabel keep: %v", err)
	}
	drop, err := c.CreateLabel(context.Background(), pid, "drop", "#222222")
	if err != nil {
		t.Fatalf("CreateLabel drop: %v", err)
	}
	if err := c.DeleteLabel(context.Background(), pid, drop.ID); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}
	labels, err := c.ListLabels(context.Background(), pid)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != 1 || labels[0].ID != keep.ID {
		t.Errorf("labels after delete = %+v, want only keep %s", labels, keep.ID)
	}
}

func TestAddBlockedByAndRelations(t *testing.T) {
	c, fake, _ := newTestClient(t)
	pid := mustProject(t, c)
	a, _ := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "a"})
	b, _ := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "b"})

	if err := c.AddBlockedBy(context.Background(), pid, a.ID, b.ID); err != nil {
		t.Fatalf("AddBlockedBy: %v", err)
	}
	fake.mu.Lock()
	post := fake.relationsPost
	fake.mu.Unlock()
	if rt, _ := post["relation_type"].(string); rt != "blocked_by" {
		t.Errorf("relation_type sent = %v, want blocked_by", post["relation_type"])
	}
	if issues, _ := post["issues"].([]any); len(issues) != 1 || issues[0] != b.ID {
		t.Errorf("issues sent = %v, want [%s]", post["issues"], b.ID)
	}

	rels, err := c.Relations(context.Background(), pid, a.ID)
	if err != nil {
		t.Fatalf("Relations: %v", err)
	}
	if len(rels.BlockedBy) != 1 || rels.BlockedBy[0].IssueID != b.ID {
		t.Errorf("blocked_by = %+v, want one ref to %s", rels.BlockedBy, b.ID)
	}
}

func TestAddComment(t *testing.T) {
	c, fake, _ := newTestClient(t)
	pid := mustProject(t, c)
	wi, _ := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "x"})

	cm, err := c.AddComment(context.Background(), pid, wi.ID, "<p>hello</p>")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if cm.CommentHTML != "<p>hello</p>" {
		t.Errorf("comment_html = %q", cm.CommentHTML)
	}
	fake.mu.Lock()
	fields := append([]string(nil), fake.commentFields...)
	fake.mu.Unlock()
	// this build only reads "comment_html"; if the client ever sends
	// "comment_text" it is silently dropped server-side.
	if !containsAll(fields, "comment_html") || containsAll(fields, "comment_text") {
		t.Errorf("comment payload fields = %v, want comment_html only", fields)
	}
}

func TestCreatePageUsesSession(t *testing.T) {
	c, fake, _ := newTestClient(t)
	pid := mustProject(t, c)

	pg, err := c.CreatePage(context.Background(), pid, PageCreate{Name: "P3 test page", ContentHTML: "<p>body</p>"})
	if err != nil {
		t.Fatalf("CreatePage: %v", err)
	}
	if pg.ID == "" || pg.Name != "P3 test page" {
		t.Errorf("page = %+v", pg)
	}
	fake.mu.Lock()
	signin, csrf, cookie := fake.signinEmail, fake.pageCSRF, fake.pageCookie
	fake.mu.Unlock()
	if signin != "bot@example.com" {
		t.Errorf("sign-in email = %q", signin)
	}
	if csrf != "csrf-post-login" {
		t.Errorf("page POST CSRF = %q, want the post-login token", csrf)
	}
	if cookie != "fakesess" {
		t.Errorf("page POST cookie = %q", cookie)
	}

	pages, err := c.ListPages(context.Background(), pid)
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if len(pages) != 1 || pages[0].Name != "P3 test page" {
		t.Errorf("ListPages = %+v", pages)
	}
}

func TestAPIErrorOn404(t *testing.T) {
	c, _, _ := newTestClient(t)
	pid := mustProject(t, c)
	_, err := c.GetWorkItem(context.Background(), pid, "wi-missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusNotFound || !strings.Contains(apiErr.Body, "Not found") {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestNewRequiresBaseURLAndWorkspace(t *testing.T) {
	if _, err := New(Config{APIKey: "k"}); err == nil {
		t.Error("New without BaseURL should fail")
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Error("New without Workspace should fail")
	}
}

func TestListWorkItemsPaginated(t *testing.T) {
	c, fake, _ := newTestClient(t)
	proj := mustProject(t, c)
	for i := 0; i < 105; i++ {
		if _, err := c.CreateWorkItem(context.Background(), proj, WorkItemCreate{Name: fmt.Sprintf("item %03d", i)}); err != nil {
			t.Fatalf("CreateWorkItem: %v", err)
		}
	}
	_ = fake
	got, err := c.ListWorkItems(context.Background(), proj)
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(got) != 105 {
		t.Fatalf("ListWorkItems returned %d items, want 105 (pagination must follow all pages)", len(got))
	}
	// sorted by sequence id
	if got[0].Name != "item 000" || got[104].Name != "item 104" {
		t.Errorf("order = %q ... %q, want item 000 ... item 104", got[0].Name, got[104].Name)
	}
}

func TestListWorkspaceMembers(t *testing.T) {
	c, _, _ := newTestClient(t)
	members, err := c.ListWorkspaceMembers(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaceMembers: %v", err)
	}
	if len(members) != 1 || members[0].Email != "bot@example.com" {
		t.Fatalf("members = %+v, want one bot@example.com member", members)
	}
}

func TestCreateState(t *testing.T) {
	c, _, _ := newTestClient(t)
	proj := mustProject(t, c)
	s, err := c.CreateState(context.Background(), proj, "Review", "started", "#FEE74C", 1500)
	if err != nil {
		t.Fatalf("CreateState: %v", err)
	}
	if s.Name != "Review" || s.Group != "started" {
		t.Errorf("state = %+v, want name Review group started", s)
	}
	states, err := c.ListStates(context.Background(), proj)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	found := false
	for _, st := range states {
		if st.ID == s.ID && st.Name == "Review" {
			found = true
		}
	}
	if !found {
		t.Errorf("created state not in ListStates: %v", states)
	}
}

func TestWebhookLifecycle(t *testing.T) {
	c, _, _ := newTestClient(t)
	wh, err := c.CreateWebhook(context.Background(), "http://bridge:8102/webhook/plane", "issue", "issue_comment")
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if wh.SecretKey == "" {
		t.Fatal("create must reveal the secret key")
	}
	if !wh.IsActive || !wh.Issue || !wh.IssueComment {
		t.Errorf("webhook flags = %+v, want active issue+issue_comment", wh)
	}

	// list must NOT include the secret
	list, err := c.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(list) != 1 || list[0].ID != wh.ID {
		t.Fatalf("ListWebhooks = %+v, want one webhook %s", list, wh.ID)
	}
	if list[0].SecretKey != "" {
		t.Errorf("list leaked the secret key")
	}

	// regenerate reveals a new secret
	wh2, err := c.RegenerateWebhookSecret(context.Background(), wh.ID)
	if err != nil {
		t.Fatalf("RegenerateWebhookSecret: %v", err)
	}
	if wh2.SecretKey == "" || wh2.SecretKey == wh.SecretKey {
		t.Errorf("regenerated secret = %q, want non-empty and different", wh2.SecretKey)
	}

	if err := c.DeleteWebhook(context.Background(), wh.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	list, err = c.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks after delete: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("list after delete = %+v, want empty", list)
	}
}

func TestWebhookCreateRejectsMissingURL(t *testing.T) {
	c, _, _ := newTestClient(t)
	_, err := c.CreateWebhook(context.Background(), "", "issue")
	if err == nil {
		t.Fatal("CreateWebhook with empty URL should fail")
	}
}

func containsAll(have []string, want ...string) bool {
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

// --- P6: item refs, board links, comments list, env config ---

func TestItemLink(t *testing.T) {
	c, _, srv := newTestClient(t)
	p, err := c.CreateProject(context.Background(), "t", "TST")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	wi, err := c.CreateWorkItem(context.Background(), p.ID, WorkItemCreate{Name: "x"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	got := c.ItemLink(p, wi)
	want := fmt.Sprintf("%s/testws/browse/TST-%d", srv.URL, wi.SequenceID)
	if got != want {
		t.Errorf("ItemLink = %q, want %q", got, want)
	}
}

func TestProjectByRef(t *testing.T) {
	c, f, _ := newTestClient(t)
	p, err := c.CreateProject(context.Background(), "t", "TST")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// by identifier (list + match)
	got, err := ProjectByRef(context.Background(), c, "TST")
	if err != nil {
		t.Fatalf("ProjectByRef(TST): %v", err)
	}
	if got.ID != p.ID || got.Identifier != "TST" {
		t.Errorf("ProjectByRef(TST) = %+v, want id %s", got, p.ID)
	}

	// by UUID (fast path, single GET)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	f.mu.Lock()
	f.projects[uuid] = map[string]string{"id": uuid, "name": "uuid proj", "identifier": "UPR"}
	f.mu.Unlock()
	got, err = ProjectByRef(context.Background(), c, uuid)
	if err != nil {
		t.Fatalf("ProjectByRef(uuid): %v", err)
	}
	if got.Identifier != "UPR" {
		t.Errorf("ProjectByRef(uuid) = %+v, want identifier UPR", got)
	}

	// unknown ref
	if _, err := ProjectByRef(context.Background(), c, "NOPE"); err == nil {
		t.Error("ProjectByRef(NOPE) should fail")
	}
}

func TestWorkItemByRef(t *testing.T) {
	c, f, _ := newTestClient(t)
	p, err := c.CreateProject(context.Background(), "t", "TST")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	wi, err := c.CreateWorkItem(context.Background(), p.ID, WorkItemCreate{Name: "one"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	// display id with prefix (list + match on sequence + identifier)
	got, err := WorkItemByRef(context.Background(), c, p.ID, "TST-1")
	if err != nil {
		t.Fatalf("WorkItemByRef(TST-1): %v", err)
	}
	if got.ID != wi.ID {
		t.Errorf("WorkItemByRef(TST-1).ID = %q, want %q", got.ID, wi.ID)
	}

	// bare sequence (no prefix check)
	got, err = WorkItemByRef(context.Background(), c, p.ID, "1")
	if err != nil {
		t.Fatalf("WorkItemByRef(1): %v", err)
	}
	if got.ID != wi.ID {
		t.Errorf("WorkItemByRef(1).ID = %q, want %q", got.ID, wi.ID)
	}

	// UUID fast path (single GET, no list)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	f.mu.Lock()
	f.items[uuid] = map[string]any{"id": uuid, "name": "uuid item", "state": "st-1", "labels": []string{}, "sequence_id": 7, "created_at": "2026-10-31T00:00:00Z"}
	f.mu.Unlock()
	got, err = WorkItemByRef(context.Background(), c, p.ID, uuid)
	if err != nil {
		t.Fatalf("WorkItemByRef(uuid): %v", err)
	}
	if got.ID != uuid || got.SequenceID != 7 {
		t.Errorf("WorkItemByRef(uuid) = %+v, want id %s seq 7", got, uuid)
	}

	// malformed ref (no dash-separated sequence)
	if _, err := WorkItemByRef(context.Background(), c, p.ID, "nope"); err == nil {
		t.Error("WorkItemByRef(nope) should fail")
	}
	// unknown sequence
	if _, err := WorkItemByRef(context.Background(), c, p.ID, "TST-99"); err == nil {
		t.Error("WorkItemByRef(TST-99) should fail")
	}
}

func TestResolveItemRef(t *testing.T) {
	c, _, _ := newTestClient(t)
	p1, err := c.CreateProject(context.Background(), "one", "TST")
	if err != nil {
		t.Fatalf("CreateProject TST: %v", err)
	}
	p2, err := c.CreateProject(context.Background(), "two", "T2I")
	if err != nil {
		t.Fatalf("CreateProject T2I: %v", err)
	}
	wi2, err := c.CreateWorkItem(context.Background(), p2.ID, WorkItemCreate{Name: "t2i item"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}

	// prefixed display id: the prefix selects the project (default is TST)
	p, itemRef, err := ResolveItemRef(context.Background(), c, "TST", "T2I-1")
	if err != nil {
		t.Fatalf("ResolveItemRef(T2I-1): %v", err)
	}
	if p.ID != p2.ID || itemRef != "T2I-1" {
		t.Errorf("ResolveItemRef(T2I-1) = (%+v, %q), want project T2I", p, itemRef)
	}
	got, err := WorkItemByRef(context.Background(), c, p.ID, itemRef)
	if err != nil || got.ID != wi2.ID {
		t.Errorf("WorkItemByRef through resolved project = (%+v, %v), want T2I item", got, err)
	}

	// prefix match is case-insensitive
	p, _, err = ResolveItemRef(context.Background(), c, "TST", "t2i-1")
	if err != nil || p.ID != p2.ID {
		t.Errorf("ResolveItemRef(t2i-1) = (%+v, %v), want project T2I", p, err)
	}

	// UUID and bare sequence: default project, ref unchanged
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	p, itemRef, err = ResolveItemRef(context.Background(), c, "TST", uuid)
	if err != nil || p.ID != p1.ID || itemRef != uuid {
		t.Errorf("ResolveItemRef(uuid) = (%+v, %q, %v), want project TST, ref unchanged", p, itemRef, err)
	}
	p, itemRef, err = ResolveItemRef(context.Background(), c, "TST", "1")
	if err != nil || p.ID != p1.ID || itemRef != "1" {
		t.Errorf("ResolveItemRef(1) = (%+v, %q, %v), want project TST, ref unchanged", p, itemRef, err)
	}

	// prefix matching no project: fall through to the default project
	p, itemRef, err = ResolveItemRef(context.Background(), c, "TST", "NOPE-1")
	if err != nil || p.ID != p1.ID || itemRef != "NOPE-1" {
		t.Errorf("ResolveItemRef(NOPE-1) = (%+v, %q, %v), want fall-through to TST", p, itemRef, err)
	}
}

func TestListComments(t *testing.T) {
	c, _, _ := newTestClient(t)
	pid := mustProject(t, c)
	wi, err := c.CreateWorkItem(context.Background(), pid, WorkItemCreate{Name: "x"})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	for _, html := range []string{"<p>first</p>", "<p>second</p>"} {
		if _, err := c.AddComment(context.Background(), pid, wi.ID, html); err != nil {
			t.Fatalf("AddComment(%s): %v", html, err)
		}
	}
	comments, err := c.ListComments(context.Background(), pid, wi.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("ListComments = %d comments, want 2", len(comments))
	}
	if comments[0].CommentHTML != "<p>first</p>" || comments[1].CommentHTML != "<p>second</p>" {
		t.Errorf("comments = %+v, want first/second in order", comments)
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(envFile, []byte("# daemon env file\nPLANE_URL = 'http://file.example:1'\nPLANE_API_KEY=\"filekey\"\nPLANE_BOT_EMAIL=bot@file.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANE_ENV_FILE", envFile)
	for _, k := range []string{"PLANE_URL", "PLANE_API_KEY", "PLANE_WORKSPACE", "PLANE_BOT_EMAIL", "PLANE_BOT_PASSWORD"} {
		t.Setenv(k, "") // force the file fallback
	}
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.baseURL != "http://file.example:1" {
		t.Errorf("baseURL = %q, want the file value", c.baseURL)
	}
	if c.apiKey != "filekey" {
		t.Errorf("apiKey = %q, want filekey", c.apiKey)
	}
	if c.email != "bot@file.example" {
		t.Errorf("email = %q, want bot@file.example", c.email)
	}
	if c.workspace != "jarvis" {
		t.Errorf("workspace = %q, want default jarvis", c.workspace)
	}

	// process env wins over the file
	t.Setenv("PLANE_URL", "http://proc.example:2")
	c2, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv (proc env): %v", err)
	}
	if c2.baseURL != "http://proc.example:2" {
		t.Errorf("baseURL = %q, want the process-env value", c2.baseURL)
	}
}
