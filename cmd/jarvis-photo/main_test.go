package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

func TestRunUsage(t *testing.T) {
	if code := run([]string{"jarvis-photo"}); code != 2 {
		t.Fatalf("no-args exit = %d, want 2", code)
	}
	if code := run([]string{"jarvis-photo", "make", "--job", "bad/../id"}); code != 2 {
		t.Fatalf("bad id exit = %d, want 2", code)
	}
}

func TestRunMissingPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	if code := run([]string{"jarvis-photo", "make", "--job", "chat-1-ab"}); code != 1 {
		t.Fatalf("missing prompt exit = %d, want 1", code)
	}
}

func TestRunEmptyPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	if err := photo.WritePrompt(dir, "chat-1-ab", "   \n"); err != nil {
		t.Fatalf("WritePrompt: %v", err)
	}
	if code := run([]string{"jarvis-photo", "make", "--job", "chat-1-ab"}); code != 1 {
		t.Fatalf("empty prompt exit = %d, want 1", code)
	}
}

func TestMakePhotoPassesSizeThrough(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/x.png","width":1024,"height":576,"seed":1,"steps":9,"seconds":20}`))
	}))
	defer srv.Close()
	t.Setenv("ART_URL", srv.URL)

	dir := t.TempDir()
	if err := photo.WritePrompt(dir, "chat-1-ab", "a wide valley"); err != nil {
		t.Fatal(err)
	}
	if err := photo.WriteSize(dir, "chat-1-ab", "16x9"); err != nil {
		t.Fatal(err)
	}
	if _, err := makePhoto(context.Background(), dir, "chat-1-ab", "a wide valley"); err != nil {
		t.Fatalf("makePhoto: %v", err)
	}
	if gotBody["size"] != "16x9" {
		t.Errorf("art service body = %v, want size 16x9", gotBody)
	}
}

func TestMakePhotoNoSizeSidecarSendsNoSize(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/x.png","width":1024,"height":1024,"seed":1,"steps":9,"seconds":20}`))
	}))
	defer srv.Close()
	t.Setenv("ART_URL", srv.URL)

	dir := t.TempDir()
	if err := photo.WritePrompt(dir, "chat-1-ab", "a red panda"); err != nil {
		t.Fatal(err)
	}
	if _, err := makePhoto(context.Background(), dir, "chat-1-ab", "a red panda"); err != nil {
		t.Fatalf("makePhoto: %v", err)
	}
	if _, hasSize := gotBody["size"]; hasSize {
		t.Errorf("no size should be sent for legacy jobs: %v", gotBody)
	}
}

func TestMakePhotoWithRefStagesAndRoutesToKlein(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/x.png","width":1024,"height":1024,"seed":1,"steps":4,"seconds":40}`))
	}))
	defer srv.Close()
	t.Setenv("ART_URL", srv.URL)
	refsDir := t.TempDir()
	t.Setenv("ART_REFS_DIR", refsDir)

	dir := t.TempDir()
	refBytes := []byte{0x89, 'P', 'N', 'G', 4, 5, 6}
	if err := photo.WritePrompt(dir, "chat-1-ab", "me at the beach"); err != nil {
		t.Fatal(err)
	}
	if err := photo.WriteRefBytes(dir, "chat-1-ab", refBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := makePhoto(context.Background(), dir, "chat-1-ab", "me at the beach"); err != nil {
		t.Fatalf("makePhoto: %v", err)
	}
	// A reference must route to the reference-capable model, with the
	// staged file name (not the sidecar path) as the ref.
	if gotBody["model"] != "klein" {
		t.Errorf("art service body = %v, want model klein", gotBody)
	}
	refs, _ := gotBody["refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want one staged name", refs)
	}
	name, _ := refs[0].(string)
	if !strings.HasPrefix(name, "ref-chat-1-ab-") || !strings.HasSuffix(name, ".png") {
		t.Errorf("staged name %q", name)
	}
	if got, err := os.ReadFile(filepath.Join(refsDir, name)); err != nil || !bytes.Equal(got, refBytes) {
		t.Errorf("staged ref = %v (err %v), want the sidecar bytes", got, err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars
	if got := truncate(long, 10); got != "abcdefghij…" {
		t.Fatalf("got %q", got)
	}
}

func TestEnsureArtLoaded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"status":"ok","model":"z","loaded":true}`))
	}))
	defer ts.Close()
	t.Setenv("ART_URL", ts.URL)
	if err := ensureArt(); err != nil {
		t.Fatalf("ensureArt: %v", err)
	}
}

func TestEnsureArtWaitsForLoad(t *testing.T) {
	// Up but not loaded: the executor waits (bounded by PHOTO_ART_WAIT_S)
	// and fails with the last health body.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"loaded":false,"error":"loading"}`))
	}))
	defer ts.Close()
	t.Setenv("ART_URL", ts.URL)
	t.Setenv("PHOTO_ART_WAIT_S", "1")
	err := ensureArt()
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "not loaded after") ||
		!strings.Contains(err.Error(), `"loaded":false`) {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureArtDownStartFails(t *testing.T) {
	// Unreachable: the executor tries `docker compose up -d art` in
	// SWAP_REPO; a directory with no compose file must surface as an
	// error, not hang or touch real containers.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	t.Setenv("ART_URL", url)
	t.Setenv("SWAP_REPO", t.TempDir())
	t.Setenv("PHOTO_ART_WAIT_S", "0")
	err := ensureArt()
	if err == nil {
		t.Fatal("want start failure")
	}
	if !strings.Contains(err.Error(), "could not be started") {
		t.Fatalf("error = %v", err)
	}
}

func TestQueueFollowupNone(t *testing.T) {
	dir := t.TempDir()
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("exit = %d, want 0 (no sidecar, no follow-up)", code)
	}
}

func TestQueueFollowupEmptyBody(t *testing.T) {
	dir := t.TempDir()
	if err := photo.WriteFollowup(dir, "chat-1-ab", "  \n"); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("exit = %d, want 0 (empty body is no follow-up)", code)
	}
}

func TestQueueFollowupPlaceholder(t *testing.T) {
	dir := t.TempDir()
	body := "driver: local\nclass: build\npriority: 900\ntitle: caption it\n\nCaption {{image}}."
	if err := photo.WriteFollowup(dir, "chat-1-ab", body); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	store, err := agentjob.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	j, err := store.Get("fu-chat-1-ab")
	if err != nil {
		t.Fatalf("follow-up job missing: %v", err)
	}
	if j.Title != "caption it" || j.Driver != "local" || j.Class != "build" || j.Priority != 900 {
		t.Fatalf("spec: title=%q driver=%q class=%q priority=%d", j.Title, j.Driver, j.Class, j.Priority)
	}
	if j.Source != "photo-followup" {
		t.Fatalf("source = %q", j.Source)
	}
	if !strings.HasPrefix(j.Instructions, "Caption /data/imgs/p1.png.") ||
		strings.Contains(j.Instructions, "{{image}}") {
		t.Fatalf("instructions = %q", j.Instructions)
	}
}

func TestQueueFollowupNoPlaceholder(t *testing.T) {
	dir := t.TempDir()
	if err := photo.WriteFollowup(dir, "chat-1-ab", "Post the result to the gallery channel."); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	store, err := agentjob.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	j, err := store.Get("fu-chat-1-ab")
	if err != nil {
		t.Fatalf("follow-up job missing: %v", err)
	}
	// No placeholder: the image path and the gallery are appended.
	if !strings.Contains(j.Instructions, "Post the result to the gallery channel.") ||
		!strings.Contains(j.Instructions, "Generated image: /data/imgs/p1.png") ||
		!strings.Contains(j.Instructions, "Gallery: ") {
		t.Fatalf("instructions = %q", j.Instructions)
	}
	// No headers: driver falls back to local, title is derived.
	if j.Driver != "local" || j.Title != "follow-up for photo chat-1-ab" {
		t.Fatalf("defaults: driver=%q title=%q", j.Driver, j.Title)
	}
}

func TestQueueFollowupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := photo.WriteFollowup(dir, "chat-1-ab", "Caption {{image}}."); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("first exit = %d", code)
	}
	// A re-run of the photo job re-enqueues the same follow-up id: Ingest
	// is idempotent, so the second attempt must not fail or duplicate.
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 0 {
		t.Fatalf("second exit = %d, want 0 (idempotent re-queue)", code)
	}
	jobs, err := agentjob.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	all, err := jobs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, x := range all {
		if x.ID == "fu-chat-1-ab" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("fu-chat-1-ab appears %d times, want 1", n)
	}
}

func TestQueueFollowupBadBody(t *testing.T) {
	dir := t.TempDir()
	if err := photo.WriteFollowup(dir, "chat-1-ab", "driver: local\n"); err != nil {
		t.Fatalf("WriteFollowup: %v", err)
	}
	if code := queueFollowup(dir, "chat-1-ab", "/data/imgs/p1.png"); code != 1 {
		t.Fatalf("exit = %d, want 1 (header-only body has no instruction text)", code)
	}
}
