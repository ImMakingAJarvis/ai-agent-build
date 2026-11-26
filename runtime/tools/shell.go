package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

// shellKeep is how much output shell keeps in memory: the last 64 KB.
// Everything is also streamed to the op's log file (Call.LogPath).
const shellKeep = 64 << 10

// Shell runs a shell command in the op's workspace (shell). The command
// runs as `bash -lc <cmd>` in its own process group, so when the op's
// context is cancelled or times out the whole group is killed, not just
// bash. The Runner owns timeouts: an explicit timeout_seconds is applied
// as-is, never capped.
type Shell struct {
	Guard *perm.Guard
}

func (Shell) Spec() ops.Spec {
	return ops.Spec{
		Name:        "shell",
		Description: "Run a shell command in the work dir (bash -lc), streaming output to the op log",
		Schema: schema(map[string]any{
			"cmd":             map[string]any{"type": "string", "description": "command line to run"},
			"timeout_seconds": map[string]any{"type": "integer", "description": "max seconds to run (optional; default the Runner's class timeout)"},
		}, "cmd"),
		Class:      ops.Normal,
		ExpectFast: false,
	}
}

func (t Shell) Run(ctx context.Context, c ops.Call, progress func()) ops.Result {
	var a struct {
		Cmd            string `json:"cmd"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil {
		return fail(fmt.Errorf("shell args: %w", err))
	}
	if a.Cmd == "" {
		return fail(errors.New("shell needs a cmd"))
	}
	if reason, blocked := t.Guard.CheckCommand(a.Cmd); blocked {
		return blockedResult(reason)
	}
	if a.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(a.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	var logFile *os.File
	if c.LogPath != "" {
		var err error
		logFile, err = os.OpenFile(c.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fail(err)
		}
		defer logFile.Close()
	}

	out := &shellOut{f: logFile, tail: newTail(shellKeep), onWrite: progress}
	cmd := exec.Command("bash", "-lc", a.Cmd)
	cmd.Dir = c.Workspace
	cmd.Stdout = out
	cmd.Stderr = out
	// Its own process group, so cancellation can kill the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	// The caller (the loop) records the PID so it can adopt the process
	// when it restarts while the command still runs (card m3-1b).
	if c.OnPID != nil {
		c.OnPID(cmd.Process.Pid)
	}
	var (
		mu       sync.Mutex
		waitErr  error
		waitDone = make(chan struct{})
	)
	go func() {
		waitErr = cmd.Wait()
		mu.Lock()
		close(waitDone)
		mu.Unlock()
	}()
	go func() {
		<-ctx.Done()
		mu.Lock()
		select {
		case <-waitDone:
			// Already gone: nothing to kill (the group, or a recycled
			// pid, is no longer ours).
			mu.Unlock()
			return
		default:
		}
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		mu.Unlock()
	}()
	<-waitDone

	obs := out.String()
	if waitErr == nil {
		return done(obs)
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ops.Result{Status: agentjob.OpFailed, ExitCode: ee.ExitCode(), Output: obs}
	}
	res := fail(waitErr)
	res.Output = obs
	return res
}

// ShellTool returns the shell tool confined by g.
func ShellTool(g *perm.Guard) ops.Tool {
	return Shell{Guard: g}
}

// shellOut fans the command's output to the op log file and a buffer that
// keeps the last shellKeep bytes, and reports each write. The exec package
// copies stdout and stderr from separate goroutines, so writes are
// serialized.
type shellOut struct {
	mu      sync.Mutex
	f       *os.File
	tail    tail
	onWrite func()
}

func (w *shellOut) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Write(p)
	}
	w.tail.write(p)
	if w.onWrite != nil {
		w.onWrite()
	}
	return len(p), nil
}

func (w *shellOut) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tail.string()
}

// tail is a ring buffer that keeps the last len(buf) bytes it has seen, in
// order.
type tail struct {
	buf  []byte
	off  int
	full bool
}

func newTail(keep int) tail { return tail{buf: make([]byte, keep)} }

func (t *tail) write(p []byte) {
	k := len(t.buf)
	if len(p) >= k {
		copy(t.buf, p[len(p)-k:])
		t.off, t.full = 0, true
		return
	}
	for len(p) > 0 {
		space := k - t.off
		if len(p) < space {
			space = len(p)
		}
		copy(t.buf[t.off:t.off+space], p[:space])
		t.off += space
		if t.off == k {
			t.off, t.full = 0, true
		}
		p = p[space:]
	}
}

func (t *tail) string() string {
	if !t.full {
		return string(t.buf[:t.off])
	}
	return string(t.buf[t.off:]) + string(t.buf[:t.off])
}
