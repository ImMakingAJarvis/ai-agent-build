// Package lock keeps one runtime at a time (card ELR-87): two runtimes
// must never schedule the same jobs, which happens when a container
// restart overlaps the old and the new loopd, or when someone runs
// 'loopctl run' while loopd is up.
//
// The lock is an exclusive flock on <job store directory>/loop.lock.
// The kernel releases the flock when the holding process dies, so a
// crash never leaves a stale lock. The file's text ("<pid> <RFC3339
// time>\n") is for a human reading it; it is not part of the lock.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock holds an exclusive flock on its file.
type Lock struct{ f *os.File }

// ErrHeld is returned by Acquire when another process holds the lock.
var ErrHeld = errors.New("another runtime holds the lock")

// Acquire takes the lock at path, creating the parent directory when it
// is missing. When the lock is already held, the file is closed and
// ErrHeld is returned.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, err
	}
	if err := f.Truncate(0); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock. Calling it twice is not an error.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

// Holder is the text in the lock file (the holder's pid and acquire
// time), "" when the file is missing.
func Holder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
