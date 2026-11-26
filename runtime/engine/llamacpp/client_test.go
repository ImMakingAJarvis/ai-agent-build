package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

// TestHealth covers a ready engine (200) and an unhealthy one (503).
func TestHealth(t *testing.T) {
	ok := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	if err := ok.Health(context.Background()); err != nil {
		t.Fatalf("Health on 200: %v", err)
	}

	down := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	err := down.Health(context.Background())
	if err == nil {
		t.Fatal("Health on 503: want error, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not mention the status", err)
	}
}

// TestSlots checks the /slots reply is mapped onto engine.SlotState.
func TestSlots(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":0,"is_processing":false,"n_prompt_tokens":120},
			{"id":1,"is_processing":true,"n_prompt_tokens":4096}]`)
	}))
	slots, err := c.Slots(context.Background())
	if err != nil {
		t.Fatalf("Slots: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	if slots[0].ID != 0 || slots[0].Busy || slots[0].Tokens != 120 {
		t.Errorf("slot 0 = %+v", slots[0])
	}
	if slots[1].ID != 1 || !slots[1].Busy || slots[1].Tokens != 4096 {
		t.Errorf("slot 1 = %+v", slots[1])
	}
}

// TestSaveSlotSendsFilename checks the request shape, the reply's
// n_saved, the timing, and the on-disk stat of the snapshot file.
func TestSaveSlotSendsFilename(t *testing.T) {
	dir := t.TempDir()
	// The engine writes the snapshot itself; plant it so the stat path
	// has something to measure.
	snap := filepath.Join(dir, "job1.bin")
	if err := os.WriteFile(snap, []byte(strings.Repeat("x", 1234)), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotPath, gotQuery, gotBody string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotBody = r.URL.Path, r.URL.Query().Get("action"), mustRead(t, r.Body)
		io.WriteString(w, `{"n_saved":512}`)
	}))
	c.SlotDir = dir

	meta, err := c.SaveSlot(context.Background(), 3, "job1.bin")
	if err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	if gotPath != "/slots/3" {
		t.Errorf("path = %q, want /slots/3", gotPath)
	}
	if gotQuery != "save" {
		t.Errorf("action = %q, want save", gotQuery)
	}
	var sent struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("body %q is not JSON: %v", gotBody, err)
	}
	if sent.Filename != "job1.bin" {
		t.Errorf("filename = %q, want job1.bin", sent.Filename)
	}
	if meta.File != "job1.bin" || meta.Tokens != 512 || meta.Bytes != 1234 {
		t.Errorf("meta = %+v, want file job1.bin tokens 512 bytes 1234", meta)
	}
	if meta.Seconds < 0 {
		t.Errorf("seconds = %f, want >= 0", meta.Seconds)
	}
}

// TestRestoreSlot checks the restore request carries the filename and
// a non-2xx status is an error.
func TestRestoreSlot(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotBody = r.URL.Path, r.URL.Query().Get("action"), mustRead(t, r.Body)
	}))
	if err := c.RestoreSlot(context.Background(), 1, "job1.bin"); err != nil {
		t.Fatalf("RestoreSlot: %v", err)
	}
	if gotPath != "/slots/1" || gotQuery != "restore" {
		t.Errorf("request = %s?%s, want /slots/1?restore", gotPath, gotQuery)
	}
	var sent struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("body %q is not JSON: %v", gotBody, err)
	}
	if sent.Filename != "job1.bin" {
		t.Errorf("filename = %q, want job1.bin", sent.Filename)
	}

	down := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such file", http.StatusNotFound)
	}))
	err := down.RestoreSlot(context.Background(), 1, "missing.bin")
	if err == nil {
		t.Fatal("RestoreSlot on 404: want error, got nil")
	}
}

// TestEraseSlot checks erase is a POST with no body.
func TestEraseSlot(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotMethod string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotBody = r.URL.Path, r.URL.Query().Get("action"), mustRead(t, r.Body)
		gotMethod = r.Method
	}))
	if err := c.EraseSlot(context.Background(), 2); err != nil {
		t.Fatalf("EraseSlot: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/slots/2" || gotQuery != "erase" {
		t.Errorf("request = %s %s?%s, want POST /slots/2?erase", gotMethod, gotPath, gotQuery)
	}
	if gotBody != "" {
		t.Errorf("erase body = %q, want empty", gotBody)
	}
}

// TestInfoFromFixtures feeds the saved /props and /v1/models replies
// from the box's engine and checks the ModelInfo mapping.
func TestInfoFromFixtures(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var file string
		switch r.URL.Path {
		case "/props":
			file = "testdata/props.json"
		case "/v1/models":
			file = "testdata/models.json"
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("read %s: %v", file, err)
			return
		}
		w.Write(b)
	}))
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ID != "/opt/jarvis-engine/models/jarvis-brain.gguf" {
		t.Errorf("ID = %q", info.ID)
	}
	if info.ContextTokens != 131072 {
		t.Errorf("ContextTokens = %d, want 131072", info.ContextTokens)
	}
	if info.Build != "b1-7170cad4d" {
		t.Errorf("Build = %q, want b1-7170cad4d", info.Build)
	}
	if info.ModelFile != "/opt/jarvis-engine/models/jarvis-brain.gguf" {
		t.Errorf("ModelFile = %q", info.ModelFile)
	}
	if info.ModelBytes != 16453443584 {
		t.Errorf("ModelBytes = %d, want 16453443584", info.ModelBytes)
	}
	if info.KVType != "" {
		t.Errorf("KVType = %q, want empty (not in either reply)", info.KVType)
	}
	if fp := info.Fingerprint(); fp == "" || len(fp) != 16 {
		t.Errorf("Fingerprint = %q, want 16 hex chars", fp)
	}
}

// TestBadSnapshotName checks empty, path-escaping and parent-ref names
// are refused before any request is made.
func TestBadSnapshotName(t *testing.T) {
	called := false
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	for _, name := range []string{"", "a/b", "../x", "a..b", ".."} {
		if _, err := c.SaveSlot(context.Background(), 0, name); err == nil {
			t.Errorf("SaveSlot(%q): want error, got nil", name)
		}
		if err := c.RestoreSlot(context.Background(), 0, name); err == nil {
			t.Errorf("RestoreSlot(%q): want error, got nil", name)
		}
	}
	if called {
		t.Error("a request was made for a bad name")
	}
}

// TestHTTPErrorIncludesBody checks non-2xx errors carry the status and
// part of the body.
func TestHTTPErrorIncludesBody(t *testing.T) {
	msg := "engine exploded while loading the model"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, msg, http.StatusInternalServerError)
	}))
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health on 500: want error, got nil")
	}
	s := err.Error()
	if !strings.Contains(s, "500") {
		t.Errorf("error %q does not mention the status", s)
	}
	if !strings.Contains(s, "engine exploded") {
		t.Errorf("error %q does not include the body", s)
	}
}

func mustRead(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
