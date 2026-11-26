package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/perm"
)

// runTool runs name with args against FileTools confined to dir.
func runTool(t *testing.T, dir, name string, args any) ops.Result {
	t.Helper()
	g := &perm.Guard{WorkDir: dir}
	tool, ok := ops.NewRegistry(FileTools(g)...).Get(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), ops.Call{Name: name, Args: b}, func() {})
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "alpha\nbeta\n")
	res := runTool(t, dir, "read_file", map[string]any{"path": "a.txt"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	for _, want := range []string{"1\talpha", "2\tbeta"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q:\n%s", want, res.Output)
		}
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	res := runTool(t, dir, "write_file", map[string]any{
		"path": "nested/deep/b.txt", "content": "hello",
	})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if res.Output != "wrote 5 bytes" {
		t.Errorf("output %q", res.Output)
	}
	got, err := os.ReadFile(filepath.Join(dir, "nested/deep/b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("file content %q", got)
	}
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "foo bar\n")
	res := runTool(t, dir, "edit_file", map[string]any{
		"path": "a.txt", "old": "foo", "new": "qux",
	})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if res.Output != "edited a.txt" {
		t.Errorf("output %q", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "qux bar\n" {
		t.Errorf("file content %q", got)
	}
}

func TestEditNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "foo\n")
	res := runTool(t, dir, "edit_file", map[string]any{
		"path": "a.txt", "old": "nope", "new": "x",
	})
	if res.Status != agentjob.OpFailed || res.Error != "old text not found" {
		t.Errorf("status %q err %q", res.Status, res.Error)
	}
}

func TestEditAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x x x\n")
	res := runTool(t, dir, "edit_file", map[string]any{
		"path": "a.txt", "old": "x", "new": "y",
	})
	if res.Status != agentjob.OpFailed {
		t.Fatalf("status %q", res.Status)
	}
	want := "old text occurs 3 times; add more context"
	if res.Error != want {
		t.Errorf("err %q, want %q", res.Error, want)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "x x x\n" {
		t.Errorf("file changed: %q", got)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.txt", "x")
	writeFile(t, dir, "sub/a.txt", "x")
	res := runTool(t, dir, "list", map[string]any{"path": "."})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	want := "b.txt\nsub/"
	if res.Output != want {
		t.Errorf("output %q, want %q", res.Output, want)
	}
}

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/a.txt", "one\ntwo match three\n")
	res := runTool(t, dir, "search", map[string]any{"pattern": "match"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	line := filepath.Join(dir, "sub/a.txt") + ":2: two match three"
	if res.Output != line {
		t.Errorf("output %q, want %q", res.Output, line)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "1\n2\n3\n4\n5")
	res := runTool(t, dir, "read_file", map[string]any{
		"path": "a.txt", "offset": 2, "limit": 2,
	})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if res.Output != "2\t2\n3\t3\n" {
		t.Errorf("output %q", res.Output)
	}
	// An offset past the end reports how many lines the file has.
	res = runTool(t, dir, "read_file", map[string]any{
		"path": "a.txt", "offset": 5, "limit": 2,
	})
	if !strings.HasSuffix(res.Output, "(file has 5 lines)") {
		t.Errorf("output %q", res.Output)
	}
}

func TestBlockedPath(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "x")
	for _, name := range []string{"read_file", "write_file", "edit_file", "list", "search"} {
		args := map[string]any{"path": outside}
		if name == "edit_file" {
			args = map[string]any{"path": outside, "old": "a", "new": "b"}
		}
		if name == "search" {
			args = map[string]any{"pattern": "x", "path": outside}
		}
		res := runTool(t, dir, name, args)
		if res.Status != agentjob.OpFailed || !strings.HasPrefix(res.Error, "BLOCKED: ") {
			t.Errorf("%s: status %q err %q", name, res.Status, res.Error)
		}
	}
}

func TestSearchSkipsBinaryAndGit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "keep.txt", "needle here\n")
	// A .git directory with a matching file must not be walked.
	writeFile(t, dir, filepath.Join(".git", "config"), "needle in git\n")
	// A binary file (NUL in the first 512 bytes) with a matching line.
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(bin, []byte("needle\x00more"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file over 1 MB with a matching line.
	big := filepath.Join(dir, "big.dat")
	if err := os.WriteFile(big, append([]byte("needle big\n"), make([]byte, 1<<20)...), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runTool(t, dir, "search", map[string]any{"pattern": "needle"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if strings.Contains(res.Output, ".git") || strings.Contains(res.Output, "bin.dat") ||
		strings.Contains(res.Output, "big.dat") {
		t.Errorf("skipped files leaked into output:\n%s", res.Output)
	}
	want := filepath.Join(dir, "keep.txt") + ":1: needle here"
	if res.Output != want {
		t.Errorf("output %q, want %q", res.Output, want)
	}
}
