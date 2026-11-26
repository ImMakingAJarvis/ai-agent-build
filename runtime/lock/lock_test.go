package lock

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAcquireRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestSecondAcquireIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.lock")
	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire: got %v, want ErrHeld", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l3, err := Acquire(path)
	if err != nil {
		t.Fatalf("third Acquire after release: %v", err)
	}
	if err := l3.Release(); err != nil {
		t.Fatalf("third Release: %v", err)
	}
}

func TestHolderText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.lock")
	if got := Holder(path); got != "" {
		t.Fatalf("Holder on missing file: got %q, want \"\"", got)
	}
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	got := Holder(path)
	fields := strings.Fields(got)
	if len(fields) != 2 {
		t.Fatalf("Holder: %q: want two fields", got)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("Holder pid %q: %v", fields[0], err)
	}
	if _, err := time.Parse(time.RFC3339, fields[1]); err != nil {
		t.Fatalf("Holder time %q: %v", fields[1], err)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("Holder: %q: want trailing newline", got)
	}
	if pid == 0 {
		t.Fatalf("Holder: pid 0")
	}
}

func TestReleaseTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}
