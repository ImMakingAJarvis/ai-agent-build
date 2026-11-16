package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// echoHandler is the built-in test capability (same contract as
// cmd/cap-echo): sleep for the configured delay, write an artifact file
// to the request's OutDir, and reply with a Result that references it
// (a reference, not the bytes — invariant 7). A remote node serving
// "echo" is how the box exercises the off-box path end to end.
type echoHandler struct {
	delay time.Duration
	node  string
}

func (h echoHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	log.Printf("capworker-remote %s: %s: echoing (delay %s)", req.Capability, req.CorrelationID, h.delay)
	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		// Worker shutting down: do not claim the request — return an
		// error so the SDK requeues it for a live worker.
		return queue.Result{}, ctx.Err()
	}
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}
	name := filepath.Join(req.OutDir, "echo-"+req.CorrelationID+".txt")
	body := fmt.Sprintf("cap.%s remote echo artifact\ncorrelation: %s\ncapability: %s\nnode: remote:%s\ngenerated: %s\n",
		req.Capability, req.CorrelationID, req.Capability, h.node, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", name, err)
	}
	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{name},
		Summary:   fmt.Sprintf("echoed after %s (remote:%s)", h.delay, h.node),
		Data:      map[string]any{"delay": h.delay.String(), "node": h.node},
	}, nil
}

// commandResult is what a command handler's program prints on stdout:
// the Result the SDK publishes to the request's ReplyTo. Status is
// "ok" (the default when empty), "error" or "timeout"; Artifacts are
// paths (under the request's OutDir), never bytes; Data is small
// structured output.
type commandResult struct {
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Error     string         `json:"error,omitempty"`
	Artifacts []string       `json:"artifacts,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// commandHandler runs one external program per request. Protocol:
// the request envelope is JSON on the program's stdin; the program
// writes a commandResult JSON to stdout; a non-zero exit is an error
// whose message is the tail of stderr. The program's environment gets
// JARVIS_CAP (the capability), JARVIS_CORR (the correlation id),
// JARVIS_OUT_DIR (the per-request artifact subdir — write artifacts
// there) and JARVIS_REPLY_TO, on top of the normal environment and the
// config's env list. The program is killed when its timeout elapses or
// the worker shuts down.
type commandHandler struct {
	command []string
	env     []string
	timeout time.Duration
}

func (h commandHandler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	log.Printf("capworker-remote %s: %s: running %v (timeout %s)", req.Capability, req.CorrelationID, h.command, h.timeout)
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, h.command[0], h.command[1:]...)
	env := append(append([]string{}, os.Environ()...), h.env...)
	env = append(env,
		"JARVIS_CAP="+req.Capability,
		"JARVIS_CORR="+req.CorrelationID,
		"JARVIS_OUT_DIR="+req.OutDir,
		"JARVIS_REPLY_TO="+req.ReplyTo,
	)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return queue.Result{}, fmt.Errorf("stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return queue.Result{}, fmt.Errorf("start %s: %w", h.command[0], err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return queue.Result{}, fmt.Errorf("encode request: %w", err)
	}
	_, _ = stdin.Write(body)
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			// Worker shutting down: do not claim the request — return an
			// error so the SDK requeues it for a live worker.
			return queue.Result{}, ctx.Err()
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return queue.Result{
				Status: queue.ResultTimeout,
				Error:  fmt.Sprintf("command %s timed out after %s: %s", h.command[0], h.timeout, tail(stderr.String(), 512)),
			}, nil
		}
		return queue.Result{
			Status: queue.ResultError,
			Error:  fmt.Sprintf("command %s failed: %v: %s", h.command[0], err, tail(stderr.String(), 512)),
		}, nil
	}
	var res commandResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return queue.Result{
			Status: queue.ResultError,
			Error:  fmt.Sprintf("command %s printed a non-JSON result: %v (stdout: %s)", h.command[0], err, tail(stdout.String(), 256)),
		}, nil
	}
	out := queue.Result{Artifacts: res.Artifacts, Summary: res.Summary, Data: res.Data, Error: res.Error}
	switch res.Status {
	case "", "ok":
		out.Status = queue.ResultOK
	case "error":
		out.Status = queue.ResultError
	case "timeout":
		out.Status = queue.ResultTimeout
	default:
		return queue.Result{
			Status: queue.ResultError,
			Error:  fmt.Sprintf("command %s printed an unknown result status %q (want ok, error or timeout)", h.command[0], res.Status),
		}, nil
	}
	return out, nil
}

// tail returns the last n characters of s (trimmed), for error messages
// that quote stderr/stdout without flooding the Result.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
