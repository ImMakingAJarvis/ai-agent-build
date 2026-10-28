package art

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGenerateSuccess(t *testing.T) {
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotPrompt = "the-prompt" // filled by the handler below
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/out/x.png","width":1024,"height":1024,"seed":42,"steps":9,"seconds":25.3}`))
	}))
	defer srv.Close()

	c := NewWithURL(srv.URL)
	img, err := c.Generate(context.Background(), "a red panda")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if img.Path != "/out/x.png" || img.Seed != 42 || img.Width != 1024 {
		t.Errorf("unexpected image: %+v", img)
	}
	if gotPrompt == "" {
		t.Error("handler did not record the request")
	}
}

func TestGenerateBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"detail":"a generation is already running"}`))
	}))
	defer srv.Close()

	_, err := NewWithURL(srv.URL).Generate(context.Background(), "p")
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

	_, err := NewWithURL(srv.URL).Generate(context.Background(), "p")
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
	_, err := c.Generate(context.Background(), "p")
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
	_, err := NewWithURL(srv.URL).Generate(ctx, "p")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}
