package superagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseActionClean(t *testing.T) {
	reply := "I will run the build now.\n<ACTION>\n{\"tool\": \"shell\", \"args\": {\"cmd\": \"go build ./...\"}}\n</ACTION>"
	a, err := ParseAction(reply)
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if a.Tool != "shell" || argStr(a.Args, "cmd", "") != "go build ./..." {
		t.Fatalf("got %+v", a)
	}
}

func TestParseActionLastBlockWins(t *testing.T) {
	reply := `<ACTION>{"tool": "shell", "args": {"cmd": "wrong"}}</ACTION>
Hmm, that was wrong.
<ACTION>
{"tool": "shell", "args": {"cmd": "right"}}
</ACTION>`
	a, err := ParseAction(reply)
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if argStr(a.Args, "cmd", "") != "right" {
		t.Fatalf("want last block, got %q", argStr(a.Args, "cmd", ""))
	}
}

func TestParseActionMissing(t *testing.T) {
	if _, err := ParseAction("I think I should just stop."); err == nil {
		t.Fatal("want error when there is no ACTION block")
	}
	if _, err := ParseAction("<ACTION>not json</ACTION>"); err == nil {
		t.Fatal("want error for non-JSON block")
	}
	if _, err := ParseAction(`<ACTION>{"args": {}}</ACTION>`); err == nil {
		t.Fatal("want error for missing tool")
	}
}

func TestArgInt(t *testing.T) {
	if got := argInt(mustArgs(`{"timeout": 30}`), "timeout", 120); got != 30 {
		t.Fatalf("timeout = %d", got)
	}
	if got := argInt(mustArgs(`{"timeout": "45"}`), "timeout", 120); got != 45 {
		t.Fatalf("string timeout = %d", got)
	}
	if got := argInt(mustArgs(`{}`), "timeout", 120); got != 120 {
		t.Fatalf("missing timeout = %d", got)
	}
}

// mustArgs wraps a JSON string as the raw args an action carries.
func mustArgs(s string) json.RawMessage { return json.RawMessage(s) }

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := firstLine(long); len(got) != 120 {
		t.Fatalf("len = %d", len(got))
	}
}
