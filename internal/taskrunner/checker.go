package taskrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checker runs the card's acceptance check — plus the repo's build and
// lint, when the repo has them — in a working directory.
type Checker interface {
	// Check reports whether the acceptance check (and the build/lint,
	// for Go repos) passed, with the combined output for the result
	// comment.
	Check(ctx context.Context, dir string, acceptance string) (bool, string)
}

// ShellChecker runs the acceptance check as a shell command in dir. A
// repo with a go.mod also has to pass go build ./... and go vet ./...
// (the PLAN.md "acceptance check plus build and lint"); any other repo is
// gated on the acceptance check alone.
type ShellChecker struct {
	// Timeout bounds each command.
	Timeout time.Duration
}

// Check implements Checker.
func (c ShellChecker) Check(ctx context.Context, dir string, acceptance string) (bool, string) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	var out strings.Builder
	pass := true
	if ok, o := runShell(ctx, dir, timeout, acceptance); !ok {
		pass = false
		out.WriteString(o)
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		for _, args := range [][]string{{"go", "build", "./..."}, {"go", "vet", "./..."}} {
			if ok, o := runCmd(ctx, dir, timeout, args...); !ok {
				pass = false
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(strings.Join(args, " ") + ":\n" + o)
			}
		}
	}
	return pass, out.String()
}

func runShell(ctx context.Context, dir string, timeout time.Duration, script string) (bool, string) {
	return runCmd(ctx, dir, timeout, append([]string{"bash", "-c"}, script)...)
}

func runCmd(ctx context.Context, dir string, timeout time.Duration, args ...string) (bool, string) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, args[0], args[1:]...).CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return false, fmt.Sprintf("%s: timed out after %s\n%s", strings.Join(args, " "), timeout, string(out))
	}
	if err != nil {
		return false, fmt.Sprintf("%s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return true, string(out)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
