package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
)

func runSearchOutput(t *testing.T, tool SearchOutput, args any) ops.Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), ops.Call{Name: "search_output", Args: b}, func() {})
}

func TestSpillSmallUntouched(t *testing.T) {
	saved := 0
	got, err := Spill("tiny output", 1024, func(string) (string, error) {
		saved++
		return "h", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spilled || got.Handle != "" {
		t.Errorf("small output was spilled: %+v", got)
	}
	if got.Text != "tiny output" {
		t.Errorf("text altered: %q", got.Text)
	}
	if saved != 0 {
		t.Errorf("save called %d times for a small output", saved)
	}
}

func TestSpillBig(t *testing.T) {
	lines := make([]string, 10000)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %04d", i+1)
	}
	lines[4999] = "ERROR boom"
	output := strings.Join(lines, "\n")

	const handle = "spill-abc123"
	got, err := Spill(output, 4096, func(s string) (string, error) {
		if s != output {
			t.Error("save did not receive the full output")
		}
		return handle, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spilled || got.Handle != handle {
		t.Fatalf("not spilled with the handle: %+v", got)
	}
	if !strings.Contains(got.Text, "[output was") {
		t.Error("summary missing the header line")
	}
	for _, want := range []string{"line 0001", "line 0040"} { // head
		if !strings.Contains(got.Text, want) {
			t.Errorf("summary missing head line %q", want)
		}
	}
	for _, want := range []string{"line 9999", "line 10000"} { // tail
		if !strings.Contains(got.Text, want) {
			t.Errorf("summary missing tail line %q", want)
		}
	}
	if !strings.Contains(got.Text, "5000: ERROR boom") {
		t.Error("summary missing the numbered error line at 5000")
	}
	if !strings.Contains(got.Text, handle) {
		t.Error("summary missing the handle")
	}
	if len(got.Text) >= 12*1024 {
		t.Errorf("summary is %d bytes, want under 12 KB", len(got.Text))
	}
}

func TestSpillLongLine(t *testing.T) {
	long := strings.Repeat("x", 1200)
	output := strings.Join([]string{"short one", long, "short two"}, "\n")
	got, err := Spill(output, 64, func(string) (string, error) { return "h", nil })
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("x", 500) + "…"
	if !strings.Contains(got.Text, want) {
		t.Error("long line not cut to 500 chars with ellipsis")
	}
	if strings.Contains(got.Text, strings.Repeat("x", 501)) {
		t.Error("a line longer than 500 chars survived")
	}
}

func TestSearchOutputPattern(t *testing.T) {
	content := "alpha\nERROR one\nbeta\nFAIL two\ngamma\n"
	tool := SearchOutput{Open: func(h string) (string, error) {
		if h != "h1" {
			t.Errorf("Open handle %q, want h1", h)
		}
		return content, nil
	}}
	res := runSearchOutput(t, tool, map[string]any{"handle": "h1", "pattern": "ERROR|FAIL"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	for _, want := range []string{"2: ERROR one", "4: FAIL two"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q:\n%s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "alpha") {
		t.Errorf("non-matching line leaked into the output:\n%s", res.Output)
	}

	// at most 100 matches
	big := strings.Repeat("boom\n", 150)
	tool = SearchOutput{Open: func(string) (string, error) { return big, nil }}
	res = runSearchOutput(t, tool, map[string]any{"handle": "h1", "pattern": "boom"})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if got := len(strings.Split(res.Output, "\n")); got != 100 {
		t.Errorf("pattern search returned %d lines, want at most 100", got)
	}
}

func TestSearchOutputRange(t *testing.T) {
	var lines []string
	for i := 1; i <= 300; i++ {
		lines = append(lines, fmt.Sprintf("l%03d", i))
	}
	content := strings.Join(lines, "\n")
	tool := SearchOutput{Open: func(string) (string, error) { return content, nil }}

	res := runSearchOutput(t, tool, map[string]any{"handle": "h", "from": 10, "to": 12})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if want := "10: l010\n11: l011\n12: l012"; res.Output != want {
		t.Errorf("range output %q, want %q", res.Output, want)
	}

	// at most 200 lines
	res = runSearchOutput(t, tool, map[string]any{"handle": "h", "from": 1})
	if res.Status != agentjob.OpDone {
		t.Fatalf("status %q (err %q)", res.Status, res.Error)
	}
	if !strings.Contains(res.Output, "200: l200") || strings.Contains(res.Output, "201: l201") {
		t.Errorf("range not capped at 200 lines:\n%s", res.Output)
	}

	// no pattern, no range
	res = runSearchOutput(t, tool, map[string]any{"handle": "h"})
	if res.Status != agentjob.OpFailed || res.Error != "give a pattern or a line range" {
		t.Errorf("neither-args result: status %q err %q", res.Status, res.Error)
	}

	// range beyond the end
	res = runSearchOutput(t, tool, map[string]any{"handle": "h", "from": 500})
	if res.Status != agentjob.OpDone || res.Output != "(no lines)" {
		t.Errorf("out-of-range result: status %q output %q err %q", res.Status, res.Output, res.Error)
	}
}

func TestSearchOutputBadHandle(t *testing.T) {
	opened := 0
	tool := SearchOutput{Open: func(string) (string, error) {
		opened++
		return "x", nil
	}}
	for _, bad := range []string{"a/b", "a..b", "/etc/passwd"} {
		res := runSearchOutput(t, tool, map[string]any{"handle": bad})
		if res.Status != agentjob.OpFailed {
			t.Errorf("handle %q: status %q, want failed", bad, res.Status)
		}
	}
	if opened != 0 {
		t.Errorf("Open called %d times for bad handles", opened)
	}
}
