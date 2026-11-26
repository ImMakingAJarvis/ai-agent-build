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

func TestParseOneBlock(t *testing.T) {
	text := "  `a/b.txt`  \n<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE\n"
	blocks, err := ParseBlockEdits(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	b := blocks[0]
	if b.Path != "a/b.txt" {
		t.Errorf("path %q, want %q (backticks and spaces stripped)", b.Path, "a/b.txt")
	}
	if b.Old != "foo" {
		t.Errorf("old %q, want %q", b.Old, "foo")
	}
	if b.New != "bar" {
		t.Errorf("new %q, want %q", b.New, "bar")
	}
}

func TestParseTwoBlocks(t *testing.T) {
	text := "a.txt\n<<<<<<< SEARCH\none\n=======\nONE\n>>>>>>> REPLACE\n" +
		"\nb.txt\n<<<<<<< SEARCH\ntwo\n=======\nTWO\n>>>>>>> REPLACE\n"
	blocks, err := ParseBlockEdits(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Path != "a.txt" || blocks[0].Old != "one" || blocks[0].New != "ONE" {
		t.Errorf("block 1 = %+v", blocks[0])
	}
	if blocks[1].Path != "b.txt" || blocks[1].Old != "two" || blocks[1].New != "TWO" {
		t.Errorf("block 2 = %+v", blocks[1])
	}
}

func TestParseCreateFile(t *testing.T) {
	text := "new.txt\n<<<<<<< SEARCH\n=======\nhello\nworld\n>>>>>>> REPLACE\n"
	blocks, err := ParseBlockEdits(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	b := blocks[0]
	if b.Path != "new.txt" {
		t.Errorf("path %q, want %q", b.Path, "new.txt")
	}
	if b.Old != "" {
		t.Errorf("old %q, want empty (create)", b.Old)
	}
	if b.New != "hello\nworld" {
		t.Errorf("new %q, want %q", b.New, "hello\nworld")
	}
}

func TestParseMissingMarker(t *testing.T) {
	// SEARCH without =======
	if _, err := ParseBlockEdits("a.txt\n<<<<<<< SEARCH\nfoo\n>>>>>>> REPLACE\n"); err == nil ||
		!strings.Contains(err.Error(), "block 1") {
		t.Errorf("missing ======: err = %v, want an error naming block 1", err)
	}
	// SEARCH without >>>>>>> REPLACE
	if _, err := ParseBlockEdits("a.txt\n<<<<<<< SEARCH\nfoo\n=======\nbar\n"); err == nil ||
		!strings.Contains(err.Error(), "block 1") {
		t.Errorf("missing REPLACE: err = %v, want an error naming block 1", err)
	}
	// no block at all
	if _, err := ParseBlockEdits("just some text\n"); err == nil {
		t.Error("expected an error for text with no blocks")
	}
	// the second block is the one that is broken: the error names it
	if _, err := ParseBlockEdits("a.txt\n<<<<<<< SEARCH\none\n=======\nONE\n>>>>>>> REPLACE\n" +
		"b.txt\n<<<<<<< SEARCH\ntwo\n>>>>>>> REPLACE\n"); err == nil ||
		!strings.Contains(err.Error(), "block 2") {
		t.Errorf("broken second block: err = %v, want an error naming block 2", err)
	}
}

func TestApplyExact(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "aaa\nbbb\nccc\n")
	g := &perm.Guard{WorkDir: dir}
	msg, err := ApplyBlockEdit(g, BlockEdit{Path: "a.txt", Old: "bbb", New: "BBB"})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "edited a.txt" {
		t.Errorf("msg %q, want %q", msg, "edited a.txt")
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "aaa\nBBB\nccc\n" {
		t.Errorf("file %q, want the one bbb replaced", string(got))
	}
}

func TestApplyNotFoundShowsCandidates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "header\nalpha\nbravo\ncharlie\ndelta\necho\n")
	g := &perm.Guard{WorkDir: dir}
	// Old differs from the file by one changed word (bravo -> BRAVO);
	// the closest window starts at line 2, and the error shows its line
	// number and its text so the model can spot the difference.
	_, err := ApplyBlockEdit(g, BlockEdit{Path: "a.txt", Old: "alpha\nBRAVO\ncharlie", New: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	e := err.Error()
	if !strings.Contains(e, "line 2") {
		t.Errorf("error does not name the right window (line 2):\n%s", e)
	}
	for _, want := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(e, want) {
			t.Errorf("error does not show the candidate text %q:\n%s", want, e)
		}
	}
	// the file is untouched
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "header\nalpha\nbravo\ncharlie\ndelta\necho\n" {
		t.Errorf("file changed on a failed apply:\n%s", string(got))
	}
}

func TestApplyAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x\ny\nx\n")
	g := &perm.Guard{WorkDir: dir}
	_, err := ApplyBlockEdit(g, BlockEdit{Path: "a.txt", Old: "x", New: "X"})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("err = %v, want an ambiguous error", err)
	}
}

func TestApplyBlockedPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "one\n")
	g := &perm.Guard{WorkDir: dir}
	_, err := ApplyBlockEdit(g, BlockEdit{Path: "/etc/passwd", Old: "", New: "x"})
	if err == nil || !strings.Contains(err.Error(), "BLOCKED") {
		t.Errorf("err = %v, want a BLOCKED error", err)
	}
	// a blocked path in a multi-block run: the tool stops and reports it
	res := applyEditsTool(t, dir, "a.txt\n<<<<<<< SEARCH\none\n=======\nONE\n>>>>>>> REPLACE\n" +
		"/etc/passwd\n<<<<<<< SEARCH\n=======\nx\n>>>>>>> REPLACE\n")
	if res.Status != agentjob.OpFailed {
		t.Fatalf("status %q, want failed", res.Status)
	}
	if !strings.Contains(res.Error, "block 2") || !strings.Contains(res.Error, "BLOCKED") {
		t.Errorf("error %q, want block 2 named with BLOCKED", res.Error)
	}
	if !strings.Contains(res.Error, "applied before it: a.txt") {
		t.Errorf("error %q, want the applied blocks reported", res.Error)
	}
	// and the first block was really applied
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "ONE\n" {
		t.Errorf("first block not applied:\n%s", string(got))
	}
}

func applyEditsTool(t *testing.T, dir, text string) ops.Result {
	t.Helper()
	tool, ok := ops.NewRegistry(ApplyEdits{Guard: &perm.Guard{WorkDir: dir}}).Get("apply_edits")
	if !ok {
		t.Fatal("apply_edits not registered")
	}
	b, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), ops.Call{Name: "apply_edits", Args: b}, func() {})
}
