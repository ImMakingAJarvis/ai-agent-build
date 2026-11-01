package plane

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Test support (used only from _test.go files): go test runs packages in
// parallel, and the live tests in this package and in internal/bridge hit
// the same board with the same bot API key — at once they 429 each other
// into their timeouts (see plane.go's rateLimitPause). AcquireLiveLock
// serializes them across processes with a file lock.
func liveLockPath() string {
	if p := os.Getenv("PLANE_LIVE_LOCK"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "jarvis-plane-live-test.lock")
}

// AcquireLiveLock waits until no other process is running a live Plane
// test, then marks the board as in use until the returned release runs
// (callers defer it). A lock left behind by a dead test (stale for more
// than 10 minutes) is stolen. If the lock is still held after timeout the
// test skips — it would only race the other test.
func AcquireLiveLock(t *testing.T, timeout time.Duration) func() {
	t.Helper()
	path := liveLockPath()
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d\n", os.Getpid())
			f.Close()
			released := false
			return func() {
				if !released {
					released = true
					os.Remove(path)
				}
			}
		}
		// Held (EEXIST) or unopenable: steal it only when it is stale.
		if info, statErr := os.Stat(path); statErr == nil &&
			time.Since(info.ModTime()) > 10*time.Minute {
			t.Logf("stealing stale live-test lock %s", path)
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			t.Skipf("another live Plane test holds %s", path)
		}
		time.Sleep(2 * time.Second)
	}
}
