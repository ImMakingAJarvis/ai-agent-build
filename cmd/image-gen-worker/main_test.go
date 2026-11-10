package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// fakeArt is a stand-in for the art service: /health reports the model
// state, /generate returns a fixed Image whose Path is /img/<basename>
// material, and /img/<name> serves the PNG bytes.
func fakeArt(t *testing.T, loaded func() bool) (*httptest.Server, *art.Image) {
	t.Helper()
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 1, 2, 3}
	img := &art.Image{Path: "/var/lib/jarvis/art/out/img-1.png", Width: 1024, Height: 576, Seed: 42, Steps: 9, Seconds: 21.5}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "loaded": loaded()})
		case r.URL.Path == "/generate":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(img)
		case r.URL.Path == "/img/img-1.png":
			w.Write(png)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, img
}

func msg(prompt any, outDir string) queue.Message {
	in := map[string]any{}
	if prompt != nil {
		in["prompt"] = prompt
	}
	return queue.Message{
		Capability:    capability,
		CorrelationID: "t1",
		Input:         in,
		OutDir:        outDir,
	}
}

func TestHandleGenerates(t *testing.T) {
	srv, _ := fakeArt(t, func() bool { return true })
	h := handler{art: art.NewWithURL(srv.URL), artWait: time.Minute}
	out := t.TempDir()

	res, err := h.Handle(context.Background(), msg("a blue square", out))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultOK {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	want := filepath.Join(out, "img-1.png")
	if len(res.Artifacts) != 1 || res.Artifacts[0] != want {
		t.Fatalf("artifacts = %v, want [%s]", res.Artifacts, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if b[0] != 0x89 || b[1] != 'P' {
		t.Fatalf("artifact is not the PNG bytes: %v", b[:4])
	}
	if res.Summary != "generated 1024x576 (seed 42, 22s)" {
		t.Errorf("summary = %q", res.Summary)
	}
	// Handle builds Data with native Go types; the forwarder reads it
	// back through JSON, where numbers are float64 (tested there).
	if got := res.Data["seed"].(int64); got != 42 {
		t.Errorf("data seed = %v", got)
	}
	if got := res.Data["seconds"].(float64); got != 21.5 {
		t.Errorf("data seconds = %v", got)
	}
}

func TestHandleRoutesRefGeneration(t *testing.T) {
	var gotRefs any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.Write([]byte(`{"loaded":true}`))
		case r.URL.Path == "/generate":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotRefs = body["refs"]
			gotModel := body["model"]
			if gotModel != "klein" {
				t.Errorf("ref generation model = %v, want klein", gotModel)
			}
			json.NewEncoder(w).Encode(&art.Image{Path: "/o/img-1.png", Width: 1024, Height: 1024, Seed: 1, Steps: 4, Seconds: 40})
		case r.URL.Path == "/img/img-1.png":
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h := handler{art: art.NewWithURL(srv.URL), artWait: time.Minute}

	req := msg("me at the beach", t.TempDir())
	req.Input["refs"] = []any{"ref-chat-1-ab-1.png"}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	refs, _ := gotRefs.([]any)
	if len(refs) != 1 || refs[0] != "ref-chat-1-ab-1.png" {
		t.Fatalf("refs forwarded = %v", gotRefs)
	}
}

func TestHandleMissingPromptErrors(t *testing.T) {
	srv, _ := fakeArt(t, func() bool { return true })
	h := handler{art: art.NewWithURL(srv.URL), artWait: time.Minute}
	_, err := h.Handle(context.Background(), msg(nil, t.TempDir()))
	if err == nil {
		t.Fatal("want an error for a missing prompt")
	}
	if _, ok := err.(*art.ErrUnavailable); ok {
		t.Fatalf("a bad prompt is not an art outage: %v", err)
	}
}

func TestHandleArtDownErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	h := handler{art: art.NewWithURL(url), artWait: time.Second}
	_, err := h.Handle(context.Background(), msg("a blue square", t.TempDir()))
	if err == nil {
		t.Fatal("want an error when the art service is down")
	}
	if _, ok := err.(*art.ErrUnavailable); !ok {
		t.Fatalf("want *art.ErrUnavailable (retryable), got %T: %v", err, err)
	}
}

func TestHandleWaitsForModelLoad(t *testing.T) {
	var loads int32
	srv, _ := fakeArt(t, func() bool { return atomic.LoadInt32(&loads) > 0 })
	go func() {
		time.Sleep(300 * time.Millisecond)
		atomic.StoreInt32(&loads, 1)
	}()
	h := handler{art: art.NewWithURL(srv.URL), artWait: 5 * time.Second}

	res, err := h.Handle(context.Background(), msg("a lighthouse", t.TempDir()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultOK {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestHandleGenerateBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.Write([]byte(`{"loaded":true}`))
		case r.URL.Path == "/generate":
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`a generation is already running`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h := handler{art: art.NewWithURL(srv.URL), artWait: time.Minute}
	_, err := h.Handle(context.Background(), msg("a blue square", t.TempDir()))
	if err == nil {
		t.Fatal("want an error for a busy art service")
	}
	var busy *art.ErrBusy
	if !asErrBusy(err, &busy) {
		t.Fatalf("want *art.ErrBusy, got %T: %v", err, err)
	}
}

func asErrBusy(err error, target **art.ErrBusy) bool {
	for err != nil {
		if b, ok := err.(*art.ErrBusy); ok {
			*target = b
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestRefsFromInput(t *testing.T) {
	if got := refsFromInput([]any{"a.png", "b.png"}); len(got) != 2 || got[0] != "a.png" {
		t.Fatalf("[]any: %v", got)
	}
	if got := refsFromInput([]string{"a.png"}); len(got) != 1 {
		t.Fatalf("[]string: %v", got)
	}
	if got := refsFromInput(nil); got != nil {
		t.Fatalf("nil: %v", got)
	}
	if got := refsFromInput("not-a-list"); got != nil {
		t.Fatalf("string: %v", got)
	}
}
