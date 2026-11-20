package planeepic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

func TestSanitizeProjectName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Test", "Test"},
		{"Full e2e test and fix of voice chat.", "Full e2e test and fix of voice chat"},
		{"Work board: Plane + Plan with model", "Work board Plane Plan with model"},
		{"Own voice channel: camera mics + direct Sonos/Google speakers (no Home Assistant)",
			"Own voice channel camera mics direct Sonos/Google speakers no Home Assistant"},
		{"NoBrakes & friends", "NoBrakes friends"},
		{"a - b", "a b"},
		{"  spaced   out  ", "spaced out"},
	}
	for _, tc := range cases {
		if got := SanitizeProjectName(tc.in); got != tc.want {
			t.Errorf("SanitizeProjectName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// memberBoard is a tiny Plane that knows two projects and three users.
// Project "closed" refuses member reads. Project "open" starts with one
// member.
type memberBoard struct {
	mu      sync.Mutex
	members map[string]map[string]bool // project id -> user id
	posts   []string
}

func TestSyncMembersAddsEveryUserToEveryProject(t *testing.T) {
	board := &memberBoard{members: map[string]map[string]bool{
		"open": {"u1": true},
	}}
	srv := httptest.NewServer(board)
	defer srv.Close()
	c, err := plane.New(plane.Config{BaseURL: srv.URL, APIKey: "k", Workspace: "jarvis"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sync, err := SyncMembers(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if sync.Projects != 2 {
		t.Fatalf("projects = %d, want 2", sync.Projects)
	}
	if len(sync.Inaccessible) != 1 || sync.Inaccessible[0] != "CLOSED" {
		t.Fatalf("inaccessible = %v, want [CLOSED]", sync.Inaccessible)
	}
	got := addedSet(sync.Added)
	want := []string{"OPEN u2", "OPEN u3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("added = %v, want %v", got, want)
	}
	again, err := SyncMembers(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Added) != 0 {
		t.Fatalf("second sync added %v, want nothing", again.Added)
	}
}

func addedSet(adds []MemberAdd) []string {
	var out []string
	for _, a := range adds {
		out = append(out, a.Project+" "+a.Email)
	}
	sort.Strings(out)
	return out
}

func (b *memberBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/workspaces/jarvis/members/" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode([]map[string]string{
			{"id": "u1", "email": "u1"},
			{"id": "u2", "email": "u2"},
			{"id": "u3", "email": "u3"},
		})
	case r.URL.Path == "/api/v1/workspaces/jarvis/projects/" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{
				{"id": "open", "name": "Open", "identifier": "OPEN"},
				{"id": "closed", "name": "Closed", "identifier": "CLOSED"},
			},
			"next_page_results": false,
		})
	case strings.HasPrefix(r.URL.Path, "/api/v1/workspaces/jarvis/projects/") && strings.HasSuffix(r.URL.Path, "/members/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/jarvis/projects/"), "/members/")
		if id == "closed" {
			http.Error(w, `{"detail":"no"}`, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet {
			b.mu.Lock()
			var out []map[string]string
			for uid := range b.members[id] {
				out = append(out, map[string]string{"id": uid, "email": uid})
			}
			b.mu.Unlock()
			json.NewEncoder(w).Encode(out)
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Member string `json:"member"`
				Role   int    `json:"role"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Role != EditorRole {
				http.Error(w, "role", http.StatusBadRequest)
				return
			}
			b.mu.Lock()
			if b.members[id] == nil {
				b.members[id] = map[string]bool{}
			}
			b.members[id][body.Member] = true
			b.posts = append(b.posts, id+" "+body.Member)
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": "mem", "role": body.Role})
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}
