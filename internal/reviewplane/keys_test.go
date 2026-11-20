package reviewplane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/secrets"
)

// TestKeys is Job 11a of docs/pr-review-build-plan.md: the five secret
// names as stored, HasKey against a temp store, and the post path that
// takes the model's own key. No test touches
// ~/.local/share/jarvis/secrets; no secret value is printed.
func TestKeys(t *testing.T) {
	t.Run("one name per model", func(t *testing.T) {
		rows := []struct {
			model string
			want  string
		}{
			{model: "sonnet", want: "plane-sonnet-bot"},
			{model: "claude-sonnet-5-5", want: "plane-sonnet-bot"},
			{model: "opus", want: "plane-opus-bot"},
			{model: "fable", want: "plane-fable-bot"},
			{model: "claude-fable-5-1", want: "plane-fable-bot"},
			{model: "haiku", want: "plane-haiku-bot"},
			{model: "qwen3.8-27b", want: "plane-jarvis-big-brain"},
		}
		for _, row := range rows {
			if got := KeyName(row.model); got != row.want {
				t.Errorf("KeyName(%q) = %q, want %q", row.model, got, row.want)
			}
		}
		// Haiku has a key and no review queue; nothing is invented.
		for _, invented := range []string{"review.haiku", "sonnet-bot", "plane-key-sonnet", "no-such-model"} {
			if got := KeyName(invented); got != "" {
				t.Errorf("KeyName(%q) = %q, want empty", invented, got)
			}
		}
	})

	t.Run("HasKey uses a temp store", func(t *testing.T) {
		store, err := secrets.Open(t.TempDir())
		if err != nil {
			t.Fatalf("secrets.Open: %v", err)
		}
		ok, err := HasKey(store, "sonnet")
		if err != nil {
			t.Fatalf("HasKey: %v", err)
		}
		if ok {
			t.Error("HasKey(sonnet) = true on an empty store, want false")
		}
		if ok, _ := HasKey(store, "review.haiku"); ok {
			t.Error("HasKey for an unknown model = true, want false")
		}
		if err := store.Put("plane-sonnet-bot", "fake-key-not-a-real-secret"); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if ok, err := HasKey(store, "sonnet"); err != nil || !ok {
			t.Errorf("HasKey(sonnet) after Put = %v, %v; want true, nil", ok, err)
		}
		if ok, _ := HasKey(store, "opus"); ok {
			t.Error("HasKey(opus) = true, want false (stored under another name)")
		}
	})

	t.Run("post signs with the model key", func(t *testing.T) {
		fake := newFakePlane(t)
		store, err := secrets.Open(t.TempDir())
		if err != nil {
			t.Fatalf("secrets.Open: %v", err)
		}
		if err := store.Put("plane-sonnet-bot", "fake-sonnet-key"); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// The shared key exists in the environment; it must not be used.
		t.Setenv("PLANE_API_KEY", "SHARED-KEY-MUST-NOT-APPEAR")
		cm, err := PostComment(context.Background(), fake.url, "jarvis", store, "sonnet", "proj-1", "item-1", "hello")
		if err != nil {
			t.Fatalf("PostComment: %v", err)
		}
		if cm.ID != "c-1" {
			t.Errorf("comment id = %q, want c-1", cm.ID)
		}
		if keys := fake.seenKeys(); len(keys) != 1 || keys[0] != "fake-sonnet-key" {
			t.Errorf("keys recorded by the client = %v, want [fake-sonnet-key]", keys)
		}
	})

	t.Run("missing key errors and does not fall back", func(t *testing.T) {
		fake := newFakePlane(t)
		store, err := secrets.Open(t.TempDir())
		if err != nil {
			t.Fatalf("secrets.Open: %v", err)
		}
		t.Setenv("PLANE_API_KEY", "SHARED-KEY-MUST-NOT-APPEAR")
		_, err = PostComment(context.Background(), fake.url, "jarvis", store, "opus", "proj-1", "item-1", "hello")
		if err == nil {
			t.Fatal("PostComment with a missing key succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "plane-opus-bot") {
			t.Errorf("error %q does not name the secret plane-opus-bot", err)
		}
		if strings.Contains(err.Error(), "SHARED-KEY-MUST-NOT-APPEAR") {
			t.Error("error mentions the shared key value")
		}
		// A missing key means the client function was not called.
		if n := fake.requests(); n != 0 {
			t.Errorf("client called %d time(s) with a missing key, want 0", n)
		}
	})
}

// fakePlane stands in for the Plane API: it records which key each request
// carried, so a test can assert the client was signed with the model's own
// key — and that it was never called at all when the key is missing.
type fakePlane struct {
	mu   sync.Mutex
	url  string
	keys []string
	n    int
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	f := &fakePlane{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.n++
		f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"c-1","comment_html":"hello","created_at":"2026-11-19T00:00:00Z","created_by":"user-sonnet"}`)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakePlane) seenKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakePlane) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}
