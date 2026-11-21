package reelq_test

// Unit tests for the broker-independent parts of reelq: job decoding,
// the pause flag, and the resourced /state poll. The full queue path
// (publish → consume → pause under a game stream) is e2e_test.go, which
// runs against the local broker and skips where none is reachable.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/reelq"
)

func TestDecodeScanJob(t *testing.T) {
	j, err := reelq.DecodeScanJob(reelq.Message{
		ID:      "scan-9-1",
		Type:    reelq.ScanType,
		Payload: map[string]string{"library_id": "7", "scan_id": "9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.LibraryID != 7 || j.ScanID != 9 {
		t.Fatalf("got %+v, want library 7 scan 9", j)
	}
	for name, payload := range map[string]map[string]string{
		"missing library_id": {"scan_id": "9"},
		"missing scan_id":    {"library_id": "7"},
		"non-numeric":        {"library_id": "abc", "scan_id": "9"},
		"empty":              {},
	} {
		if _, err := reelq.DecodeScanJob(reelq.Message{Payload: payload}); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
}

func TestPauseFlag(t *testing.T) {
	c := reelq.NewConsumer("amqp://127.0.0.1:1", nil)
	if c.IsPaused() {
		t.Fatal("a fresh consumer must not be paused")
	}
	c.Pause()
	if !c.IsPaused() {
		t.Fatal("Pause did not stick")
	}
	c.Resume()
	if c.IsPaused() {
		t.Fatal("Resume did not clear the hold")
	}
}

func TestStreamActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"stream_active":true}`))
	}))
	defer srv.Close()
	if !reelq.StreamActive(context.Background(), srv.URL) {
		t.Fatal("stream_active:true must report active")
	}

	srvOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stream_active":false}`))
	}))
	defer srvOff.Close()
	if reelq.StreamActive(context.Background(), srvOff.URL) {
		t.Fatal("stream_active:false must report inactive")
	}

	// A resourced that is down is not an error: the poll fails open, like
	// the stack's lease broker, and the event subscription drives later
	// pauses.
	if reelq.StreamActive(context.Background(), "http://127.0.0.1:1") {
		t.Fatal("a down resourced must report inactive")
	}
}
