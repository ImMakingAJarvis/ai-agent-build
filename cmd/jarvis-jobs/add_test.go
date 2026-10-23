package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestCmdAddAndDuplicate(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdAdd(s, []string{"-id", "chat-1", "-title", "T", "-driver", "local", "-source", "discord", "-priority", "80"}, strings.NewReader("do it")); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "T" || j.Source != "discord" || j.Instructions != "do it" {
		t.Fatalf("%+v", j.Spec)
	}
	if err := cmdAdd(s, []string{"-id", "chat-1", "-title", "T"}, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("want 1 job after duplicate, got %d", len(list))
	}
}
