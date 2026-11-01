package planlabels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// fakeLabels is an in-memory stand-in for the v1 project-label endpoints
// (list, create, delete) — all Sync needs. It counts create/delete calls
// so idempotency is observable.
type fakeLabels struct {
	mu      sync.Mutex
	labels  map[string][]map[string]string // projectID → labels
	created int
	deleted int
}

func newFakeLabels() *fakeLabels {
	return &fakeLabels{labels: map[string][]map[string]string{}}
}

func (f *fakeLabels) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "testkey" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rem := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/testws/projects/")
		parts := strings.Split(strings.Trim(rem, "/"), "/")
		// parts: [pid "labels" (lid)]
		if len(parts) < 2 || parts[1] != "labels" {
			http.NotFound(w, r)
			return
		}
		pid, lid := parts[0], ""
		if len(parts) == 3 {
			lid = parts[2]
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && lid == "":
			out := append([]map[string]string(nil), f.labels[pid]...)
			json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
		case r.Method == http.MethodPost && lid == "":
			var body struct {
				Name  string `json:"name"`
				Color string `json:"color"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			l := map[string]string{"id": fmt.Sprintf("lbl-%d", len(f.labels[pid])+1), "name": body.Name, "color": body.Color}
			f.labels[pid] = append(f.labels[pid], l)
			f.created++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(l)
		case r.Method == http.MethodDelete && lid != "":
			for i, l := range f.labels[pid] {
				if l["id"] == lid {
					f.labels[pid] = append(f.labels[pid][:i], f.labels[pid][i+1:]...)
					break
				}
			}
			f.deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeLabels) names(t *testing.T, pid string) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.labels[pid] {
		out = append(out, l["name"])
	}
	return out
}

func (f *fakeLabels) counts() (created, deleted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, f.deleted
}

// testClient points a plane.Client at the fake.
func testClient(t *testing.T, f *fakeLabels) *plane.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := plane.New(plane.Config{BaseURL: srv.URL, APIKey: "testkey", Workspace: "testws"})
	if err != nil {
		t.Fatalf("plane.New: %v", err)
	}
	return c
}

// registryDoc builds a minimal valid registry with two planner models.
func registryDoc(one, two bool) string {
	enabled := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	return fmt.Sprintf(`models:
  - id: m-one
    display_name: One
    location: local
    engine: ollama
    endpoint: http://localhost:1/v1
    roles: [planner]
    context_tokens: 1000
    vram_gb: 1
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: %s
    home_location: gpu
    fallback_location: cpu
  - id: m-two
    display_name: Two
    location: local
    engine: ollama
    endpoint: http://localhost:2/v1
    roles: [planner]
    context_tokens: 1000
    vram_gb: 1
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: %s
    home_location: gpu
    fallback_location: cpu
`, enabled(one), enabled(two))
}

func parseDoc(t *testing.T, doc string) *registry.File {
	t.Helper()
	f, err := registry.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("registry.Parse: %v", err)
	}
	return f
}

var quietLog = slog.New(slog.DiscardHandler)

func TestSyncCreatesMissingAndRemovesStale(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()

	// Pre-seed: one stale plan: label and one foreign label.
	fake.mu.Lock()
	fake.labels["p1"] = []map[string]string{
		{"id": "lbl-stale", "name": "plan:m-old", "color": "#000000"},
		{"id": "lbl-epic", "name": "epic", "color": "#6366F1"},
	}
	fake.mu.Unlock()

	err := Sync(ctx, c, []string{"p1"}, parseDoc(t, registryDoc(true, false)), quietLog)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := fake.names(t, "p1")
	// Seeded order minus the stale label, new labels appended.
	want := []string{"epic", "plan:m-one"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("labels = %v, want %v (epic label must survive)", got, want)
	}
	created, deleted := fake.counts()
	if created != 1 || deleted != 1 {
		t.Errorf("counts = %d created / %d deleted, want 1 / 1", created, deleted)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()
	f := parseDoc(t, registryDoc(true, true))

	for i, label := range []string{"plan:m-one", "plan:m-two"} {
		_ = i
		fake.mu.Lock()
		fake.labels["p1"] = append(fake.labels["p1"], map[string]string{"id": fmt.Sprintf("lbl-%d", i+1), "name": label, "color": Color})
		fake.mu.Unlock()
	}
	if err := Sync(ctx, c, []string{"p1"}, f, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if created, deleted := fake.counts(); created != 0 || deleted != 0 {
		t.Errorf("second Sync changed the board: %d created, %d deleted; want 0/0", created, deleted)
	}
}

func TestSyncCoversEveryProject(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()
	f := parseDoc(t, registryDoc(true, false))

	// p2 already has the label; p1 does not.
	fake.mu.Lock()
	fake.labels["p2"] = []map[string]string{{"id": "lbl-1", "name": "plan:m-one", "color": Color}}
	fake.mu.Unlock()

	if err := Sync(ctx, c, []string{"p1", "p2"}, f, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := fake.names(t, "p1"); len(got) != 1 || got[0] != "plan:m-one" {
		t.Errorf("p1 labels = %v, want [plan:m-one]", got)
	}
	if created, deleted := fake.counts(); created != 1 || deleted != 0 {
		t.Errorf("counts = %d/%d, want 1 created / 0 deleted (p2 already synced)", created, deleted)
	}
}

func TestSyncNoPlannersRemovesAllPlanLabels(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()
	fake.mu.Lock()
	fake.labels["p1"] = []map[string]string{
		{"id": "lbl-1", "name": "plan:m-one", "color": Color},
		{"id": "lbl-2", "name": "epic", "color": "#6366F1"},
	}
	fake.mu.Unlock()

	// No enabled planner at all (both disabled).
	if err := Sync(ctx, c, []string{"p1"}, parseDoc(t, registryDoc(false, false)), quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := fake.names(t, "p1"); len(got) != 1 || got[0] != "epic" {
		t.Errorf("labels = %v, want [epic]", got)
	}
}

// --- Watcher -------------------------------------------------------------------

// writeDoc replaces the watched file's contents. Each call uses content of
// a different length, so mtime and size both change.
func writeDoc(t *testing.T, path, doc string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write models file: %v", err)
	}
}

func TestWatcherSyncsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	writeDoc(t, path, registryDoc(true, false))

	ch := make(chan bool, 8) // one entry per sync: the second model's enabled state
	w := NewWatcher(path, 10*time.Millisecond, quietLog, func(ctx context.Context, f *registry.File) error {
		plan, _ := f.ByRole("planner")
		ch <- len(plan) == 2
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer cancel()

	if got := <-ch; got { // first sync: only m-one enabled
		t.Fatalf("first sync saw m-two enabled")
	}
	writeDoc(t, path, registryDoc(true, true))
	for i := 0; i < 200; i++ { // wait for the change to be picked up
		select {
		case got := <-ch:
			if !got {
				t.Fatalf("sync after enable saw only one planner")
			}
			return
		case <-time.After(2 * time.Second):
			t.Fatal("watcher did not re-sync after the file changed")
		}
	}
}

func TestWatcherRetriesFailedSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	writeDoc(t, path, registryDoc(true, false))

	fails := 2
	ch := make(chan struct{}, 8)
	w := NewWatcher(path, 10*time.Millisecond, quietLog, func(ctx context.Context, f *registry.File) error {
		ch <- struct{}{}
		if fails > 0 {
			fails--
			return fmt.Errorf("plane is down")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	for i := 0; i < 3; i++ { // initial attempt + two retries
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("sync attempt %d did not happen (retry on failure broken)", i+1)
		}
	}
}

func TestWatcherBadFileKeepsLastGoodState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	writeDoc(t, path, registryDoc(true, false))

	var got bool // last synced file's m-two state
	var n int
	ch := make(chan struct{}, 8)
	w := NewWatcher(path, 10*time.Millisecond, quietLog, func(ctx context.Context, f *registry.File) error {
		n++
		plan, _ := f.ByRole("planner")
		got = len(plan) == 2
		ch <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	<-ch // initial sync

	// An invalid file must not be synced and must not break the watch.
	writeDoc(t, path, "models: [not a valid registry")
	// Then a valid one with m-two enabled: the watch must recover.
	writeDoc(t, path, registryDoc(true, true))
	for i := 0; i < 200; i++ {
		select {
		case <-ch:
			if n == 2 && got {
				return // synced the new good file
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher did not recover from the bad file (syncs=%d)", n)
		}
	}
}
