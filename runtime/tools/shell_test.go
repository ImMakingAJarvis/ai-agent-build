package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

// runShell runs the shell tool with args against dir, streaming to a log
// file in dir.
func runShell(t *testing.T, dir string, args map[string]any) ops.Result {
	return runShellCtx(t, dir, context.Background(), args)
}

// runShellCtx is runShell with a caller-provided context.
func runShellCtx(t *testing.T, dir string, ctx context.Context, args map[string]any) ops.Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return ShellTool(&perm.Guard{WorkDir: dir}).Run(ctx,
		ops.Call{Args: b, Workspace: dir, LogPath: filepath.Join(dir, "shell.log")}, func() {})
}

func TestShellEcho(t *testing.T) {
	dir := t.TempDir()
	res := runShell(t, dir, map[string]any{"cmd": "echo hello reel"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello reel") {
		t.Errorf("output %q", res.Output)
	}
}

func TestShellExitCode(t *testing.T) {
	dir := t.TempDir()
	res := runShell(t, dir, map[string]any{"cmd": "exit 7"})
	if res.Status != agentjob.OpFailed {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code %d, want 7", res.ExitCode)
	}
}

// TestShellWritesExitFile: the wrapper records the command's exit code
// in <log>.exit on disk (card ELR-90), and the result carries the same
// real code.
func TestShellWritesExitFile(t *testing.T) {
	dir := t.TempDir()
	res := runShell(t, dir, map[string]any{"cmd": "exit 7"})
	b, err := os.ReadFile(filepath.Join(dir, "shell.log.exit"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "7" {
		t.Errorf(".exit holds %q, want 7", got)
	}
	if res.ExitCode != 7 {
		t.Errorf("Result.ExitCode %d, want 7", res.ExitCode)
	}
}

func TestShellBlocked(t *testing.T) {
	dir := t.TempDir()
	res := runShell(t, dir, map[string]any{"cmd": "rm -rf /"})
	if res.Status != agentjob.OpFailed || !strings.HasPrefix(res.Error, "BLOCKED: ") {
		t.Fatalf("status %q err %q", res.Status, res.Error)
	}
}

func TestShellWritesLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "shell.log")
	res := runShell(t, dir, map[string]any{
		"cmd": "echo to-stdout; echo to-stderr 1>&2",
	})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"to-stdout", "to-stderr"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	// Both streams land in the kept output as well.
	if !strings.Contains(res.Output, "to-stdout") || !strings.Contains(res.Output, "to-stderr") {
		t.Errorf("output %q", res.Output)
	}
}

func TestShellCancelKillsGroup(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	args := map[string]any{"cmd": "sleep 31.337 & sleep 31.337"}
	done := make(chan ops.Result, 1)
	go func() { done <- runShellCtx(t, dir, ctx, args) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Status != agentjob.OpFailed && res.Status != agentjob.OpCancelled {
			t.Errorf("status %q", res.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not return within 2 s of cancel")
	}
	// The whole group must be gone: bash -lc and both sleeps. pgrep exits
	// 1 when nothing matches.
	out, err := exec.Command("pgrep", "-f", "sleep 31.337").Output()
	if err == nil {
		t.Errorf("sleeps still running after cancel:\n%s", out)
	}
}
