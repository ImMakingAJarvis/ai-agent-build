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
	if j.ReasoningEffort != "medium" {
		t.Fatalf("default effort = %q, want medium", j.ReasoningEffort)
	}
	if err := cmdAdd(s, []string{"-id", "chat-1", "-title", "T"}, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("want 1 job after duplicate, got %d", len(list))
	}
	if err := cmdAdd(s, []string{"-id", "chat-hi", "-title", "H", "-effort", "high"}, strings.NewReader("think")); err != nil {
		t.Fatal(err)
	}
	h, err := s.Get("chat-hi")
	if err != nil {
		t.Fatal(err)
	}
	if h.ReasoningEffort != "xhigh" {
		t.Fatalf("high maps to xhigh for the local template, got %q", h.ReasoningEffort)
	}
}

func TestCmdAddDepends(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdAdd(s, []string{"-id", "p1", "-title", "First", "-driver", "local"}, strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if err := cmdAdd(s, []string{"-id", "p2", "-title", "Second", "-depends", "p1, p3 "}, strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	j, err := s.Get("p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(j.DependsOn) != 2 || j.DependsOn[0] != "p1" || j.DependsOn[1] != "p3" {
		t.Fatalf("depends = %v", j.DependsOn)
	}
	if err := cmdAdd(s, []string{"-id", "p4", "-title", "None", "-depends", " , "}, strings.NewReader("c")); err != nil {
		t.Fatal(err)
	}
	j4, err := s.Get("p4")
	if err != nil {
		t.Fatal(err)
	}
	if len(j4.DependsOn) != 0 {
		t.Fatalf("depends = %v, want none", j4.DependsOn)
	}
}
