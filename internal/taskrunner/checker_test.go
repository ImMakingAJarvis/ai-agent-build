package taskrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestShellCheckerRunsInDir is the 2026-11-08 TST-5 regression: runCmd
// must set cmd.Dir. Before the fix every check ran in the service's own
// cwd ("/" — the unit has no WorkingDirectory), so the acceptance check
// probed the wrong tree and the go build/vet gate died with "directory
// prefix / does not contain main module" for every card in a Go repo.
func TestShellCheckerRunsInDir(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":     "module e2echk\n\ngo 1.26.0\n",
		"chk.go":     "package chk\n\nvar V = 1\n",
		"marker.txt": "ok\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// go list -m prints the module of the directory the command runs in:
	// it can only pass when the check really runs in dir, not in the
	// test process's cwd. The go.mod also arms the go build/vet gate, so
	// that path runs in dir too.
	check := "test -f marker.txt && go list -m | grep -qx e2echk"
	c := ShellChecker{Timeout: time.Minute}
	if ok, out := c.Check(context.Background(), dir, check); !ok {
		t.Fatalf("Check in %s failed:\n%s", dir, out)
	}
}
