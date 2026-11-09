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

func TestTruncatePrompt(t *testing.T) {
	if got := truncatePrompt("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars
	if got := truncatePrompt(long, 10); got != "abcdefghij…" {
		t.Fatalf("got %q", got)
	}
}
