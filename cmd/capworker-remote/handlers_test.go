package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func req(outDir string) queue.Message {
	return queue.Message{
		Capability:    "testcap",
		CorrelationID: "corr-1",
		ReplyTo:       "flow.job-1",
		OutDir:        outDir,
		Payload:       map[string]string{"x": "y"},
	}
}

// writeScript drops a shell script in a temp dir and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prog.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEchoHandler(t *testing.T) {
	outDir := t.TempDir()
	h := echoHandler{delay: 10 * time.Millisecond, node: "node2"}
	res, err := h.Handle(context.Background(), req(outDir))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultOK || len(res.Artifacts) != 1 {
		t.Fatalf("result = %+v", res)
	}
	data, err := os.ReadFile(res.Artifacts[0])
	if err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	if !strings.Contains(string(data), "corr-1") || !strings.Contains(string(data), "remote:node2") {
		t.Errorf("artifact body %q", data)
	}
}

func TestEchoHandlerContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := echoHandler{delay: time.Minute, node: "node2"}
	if _, err := h.Handle(ctx, req(t.TempDir())); err == nil {
		t.Fatal("a canceled context must return an error (the SDK requeues)")
	}
}

const successScript = `#!/bin/sh
cat > /dev/null
printf 'hello from %s' "$JARVIS_CAP" > "$JARVIS_OUT_DIR/out.txt"
printf '{"status":"ok","summary":"did it","artifacts":["%s/out.txt"],"data":{"n":1}}' "$JARVIS_OUT_DIR"
`

func TestCommandHandlerSuccess(t *testing.T) {
	outDir := t.TempDir()
	script := writeScript(t, successScript)
	h := commandHandler{command: []string{"/bin/sh", script}, timeout: 10 * time.Second}
	res, err := h.Handle(context.Background(), req(outDir))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultOK || res.Summary != "did it" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0] != filepath.Join(outDir, "out.txt") {
		t.Errorf("artifacts = %v", res.Artifacts)
	}
	data, err := os.ReadFile(res.Artifacts[0])
	if err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	if want := "hello from testcap"; string(data) != want {
		t.Errorf("artifact %q, want %q (JARVIS_CAP env not passed?)", data, want)
	}
	if res.Data["n"] != float64(1) {
		t.Errorf("data = %v", res.Data)
	}
}

func TestCommandHandlerExitFailure(t *testing.T) {
	script := writeScript(t, "#!/bin/sh\necho 'boom: disk full' >&2\nexit 1\n")
	h := commandHandler{command: []string{"/bin/sh", script}, timeout: 10 * time.Second}
	res, err := h.Handle(context.Background(), req(t.TempDir()))
	if err != nil {
		t.Fatalf("a failed command is a Result, not a handler error: %v", err)
	}
	if res.Status != queue.ResultError {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Error, "boom: disk full") {
		t.Errorf("error %q must carry the stderr tail", res.Error)
	}
}

func TestCommandHandlerTimeout(t *testing.T) {
	h := commandHandler{command: []string{"/bin/sleep", "10"}, timeout: 100 * time.Millisecond}
	start := time.Now()
	res, err := h.Handle(context.Background(), req(t.TempDir()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultTimeout {
		t.Fatalf("result = %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s — the command was not killed", elapsed)
	}
}

func TestCommandHandlerNonJSON(t *testing.T) {
	script := writeScript(t, "#!/bin/sh\ncat > /dev/null\necho 'just text'\n")
	h := commandHandler{command: []string{"/bin/sh", script}, timeout: 10 * time.Second}
	res, err := h.Handle(context.Background(), req(t.TempDir()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultError || !strings.Contains(res.Error, "non-JSON") {
		t.Errorf("result = %+v", res)
	}
}

func TestCommandHandlerUnknownStatus(t *testing.T) {
	script := writeScript(t, "#!/bin/sh\ncat > /dev/null\nprintf '{\"status\":\"maybe\"}'\n")
	h := commandHandler{command: []string{"/bin/sh", script}, timeout: 10 * time.Second}
	res, err := h.Handle(context.Background(), req(t.TempDir()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Status != queue.ResultError || !strings.Contains(res.Error, "maybe") {
		t.Errorf("result = %+v", res)
	}
}

func TestTail(t *testing.T) {
	if got := tail("short", 10); got != "short" {
		t.Errorf("tail short = %q", got)
	}
	long := strings.Repeat("x", 100)
	got := tail("  "+long+"\n", 10)
	if got != "..."+strings.Repeat("x", 10) {
		t.Errorf("tail long = %q", got)
	}
}
