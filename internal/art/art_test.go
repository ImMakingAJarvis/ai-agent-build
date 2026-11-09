package art

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGalleryURLDefaultsToLANAddress(t *testing.T) {
	t.Setenv("ART_GALLERY_URL", "")
	u := GalleryURL()
	if u != DefaultGallery {
		t.Errorf("GalleryURL() = %q, want default %q", u, DefaultGallery)
	}
	if strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") {
		t.Errorf("owner-facing gallery link must be reachable from the LAN, got %q", u)
	}
}

func TestGalleryURLEnvOverride(t *testing.T) {
	t.Setenv("ART_GALLERY_URL", "http://10.0.0.5:8120")
	if got := GalleryURL(); got != "http://10.0.0.5:8120" {
		t.Errorf("GalleryURL() = %q, want the ART_GALLERY_URL override", got)
	}
}

func TestGenerateSuccess(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/out/x.png","width":1024,"height":1024,"seed":42,"steps":9,"seconds":25.3}`))
	}))
	defer srv.Close()

	c := NewWithURL(srv.URL)
	img, err := c.Generate(context.Background(), "a red panda", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if img.Path != "/out/x.png" || img.Seed != 42 || img.Width != 1024 {
		t.Errorf("unexpected image: %+v", img)
	}
	if gotBody == nil || gotBody["prompt"] != "a red panda" {
		t.Errorf("handler body = %v", gotBody)
	}
	if _, hasSize := gotBody["size"]; hasSize {
		t.Errorf("no size should be sent for the default: %v", gotBody)
	}
}

func TestGenerateSendsSize(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/out/x.png","width":1024,"height":576,"seed":42,"steps":9,"seconds":25.3}`))
	}))
	defer srv.Close()

	img, err := NewWithURL(srv.URL).Generate(context.Background(), "a wide valley", "16x9")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotBody["size"] != "16x9" {
		t.Errorf("size not passed through: %v", gotBody)
	}
	if img.Width != 1024 || img.Height != 576 {
		t.Errorf("unexpected dimensions: %+v", img)
	}
}

func TestGenerateBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"detail":"a generation is already running"}`))
	}))
	defer srv.Close()

	_, err := NewWithURL(srv.URL).Generate(context.Background(), "p", "")
	var busy *ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestGenerateOOM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"detail":"CUDA OOM — the brain is using too much VRAM right now; try again later"}`))
	}))
	defer srv.Close()

	_, err := NewWithURL(srv.URL).Generate(context.Background(), "p", "")
	var unavail *ErrUnavailable
	if !errors.As(err, &unavail) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if unavail.Detail == "" {
		t.Error("OOM detail lost")
	}
}

func TestGenerateUnreachable(t *testing.T) {
	// Nothing listens on this port.
	c := NewWithURL("http://127.0.0.1:1")
	_, err := c.Generate(context.Background(), "p", "")
	var unavail *ErrUnavailable
	if !errors.As(err, &unavail) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestGenerateTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewWithURL(srv.URL).Generate(ctx, "p", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"1024x1024", 1024, 1024, true},
		{"1024x576", 1024, 576, true},
		{"16x9", 16, 9, true},
		{"16:9", 16, 9, true},
		{"9x16", 9, 16, true},
		{"1x1", 1, 1, true},
		{"  4x3  ", 4, 3, true},
		{"sunset", 0, 0, false},
		{"1024", 0, 0, false},
		{"0x0", 0, 0, false},
		{"1024x", 0, 0, false},
		{"xx", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		w, h, ok := ParseSize(c.in)
		if ok != c.ok || (ok && (w != c.w || h != c.h)) {
			t.Errorf("ParseSize(%q) = %d, %d, %v; want %d, %d, %v", c.in, w, h, ok, c.w, c.h, c.ok)
		}
	}
}

func TestGenerateRefSendsModelAndRefs(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/out/x.png","width":1024,"height":1024,"seed":7,"steps":4,"seconds":40}`))
	}))
	defer srv.Close()

	img, err := NewWithURL(srv.URL).GenerateRef(context.Background(), "me at the beach", "16x9", []string{"ref-chat-1-ab-1.png"})
	if err != nil {
		t.Fatalf("GenerateRef: %v", err)
	}
	if img.Seed != 7 {
		t.Errorf("unexpected image: %+v", img)
	}
	// A reference must route to the reference-capable model, with the
	// staged name passed through.
	if gotBody["model"] != "klein" {
		t.Errorf("body model = %v, want klein", gotBody["model"])
	}
	refs, _ := gotBody["refs"].([]any)
	if len(refs) != 1 || refs[0] != "ref-chat-1-ab-1.png" {
		t.Errorf("body refs = %v", gotBody["refs"])
	}
	if gotBody["size"] != "16x9" {
		t.Errorf("body size = %v", gotBody["size"])
	}
}

func TestGenerateRefNoRefsOmitsRefsKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/out/x.png","width":1024,"height":1024,"seed":7,"steps":4,"seconds":40}`))
	}))
	defer srv.Close()

	if _, err := NewWithURL(srv.URL).GenerateRef(context.Background(), "a red panda", "", nil); err != nil {
		t.Fatalf("GenerateRef: %v", err)
	}
	if _, has := gotBody["refs"]; has {
		t.Errorf("no refs should be sent: %v", gotBody)
	}
}

func TestFetchRef(t *testing.T) {
	want := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refs/ref-chat-1-ab-1.png" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(want)
	}))
	defer srv.Close()

	got, err := NewWithURL(srv.URL).FetchRef(context.Background(), "ref-chat-1-ab-1.png")
	if err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFetchRefMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such ref", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := NewWithURL(srv.URL).FetchRef(context.Background(), "nope.png"); err == nil {
		t.Fatal("expected error for missing ref")
	}
}
