package inbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendPendingDrain(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())

	if n := Count(); n != 0 {
		t.Fatalf("fresh inbox has %d pending", n)
	}
	if got, err := Drain(); err != nil || got != "" {
		t.Fatalf("empty drain: %q, %v", got, err)
	}

	// Two messages, oldest first.
	if _, err := Send("first"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct UnixNano names
	if _, err := Send("second"); err != nil {
		t.Fatal(err)
	}
	if n := Count(); n != 2 {
		t.Fatalf("want 2 pending, got %d", n)
	}
	p, err := Pending()
	if err != nil || len(p) != 2 {
		t.Fatalf("pending: %v, %v", p, err)
	}
	got, err := Drain()
	if err != nil {
		t.Fatal(err)
	}
	want := "first\n\nsecond"
	if got != want {
		t.Fatalf("drain = %q, want %q", got, want)
	}
	if n := Count(); n != 0 {
		t.Fatalf("after drain: %d pending", n)
	}
	if got, _ := Drain(); got != "" {
		t.Fatalf("second drain = %q", got)
	}
	// Delivered files are kept for history.
	delivered := filepath.Join(Dir(), "delivered")
	ents, err := os.ReadDir(delivered)
	if err != nil || len(ents) != 2 {
		t.Fatalf("delivered dir: %v, %v", ents, err)
	}
}

func TestSendValidation(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	for _, bad := range []string{"", "   \n"} {
		if _, err := Send(bad); err == nil {
			t.Errorf("Send(%q) should fail", bad)
		}
	}
	if _, err := Send(strings.Repeat("x", MaxText+1)); err == nil {
		t.Error("oversized message accepted")
	}
	if _, err := Send(strings.Repeat("x", MaxText)); err != nil {
		t.Errorf("max-size message rejected: %v", err)
	}
	Drain() // clear it so the trim check below sees only "hi"
	// Send trims surrounding whitespace.
	if _, err := Send("  hi  \n"); err != nil {
		t.Fatal(err)
	}
	got, _ := Drain()
	if got != "hi" {
		t.Fatalf("trimmed message = %q", got)
	}
}

func TestDrainIgnoresNonMessages(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	dir := Dir()
	os.MkdirAll(filepath.Join(dir, "delivered"), 0o755)
	os.WriteFile(filepath.Join(dir, "notes.tmp"), []byte("nope"), 0o644)
	os.WriteFile(filepath.Join(dir, "delivered", "old.txt"), []byte("nope"), 0o644)
	if _, err := Send("real"); err != nil {
		t.Fatal(err)
	}
	got, err := Drain()
	if err != nil || got != "real" {
		t.Fatalf("drain = %q, %v", got, err)
	}
}

func TestPruneDelivered(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	delivered := filepath.Join(Dir(), "delivered")
	os.MkdirAll(delivered, 0o755)
	base := time.Now().UnixNano()
	for i := 0; i < keepDelivered+10; i++ {
		name := filepath.Join(delivered, fmt.Sprintf("%d-0000.txt", base+int64(i)))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Send("new"); err != nil {
		t.Fatal(err)
	}
	if _, err := Drain(); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(delivered)
	if len(ents) != keepDelivered {
		t.Fatalf("delivered count = %d, want %d", len(ents), keepDelivered)
	}
	// 211 delivered minus 200 kept: the oldest eleven (base+0..base+10)
	// are gone, the rest including "new" survives.
	if _, err := os.Stat(filepath.Join(delivered, fmt.Sprintf("%d-0000.txt", base+11))); err != nil {
		t.Fatalf("oldest survivor missing: %v", err)
	}
}
