package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func seedJobs(t *testing.T, s *agentjob.Store, id, status string) {
	t.Helper()
	if err := cmdAdd(s, []string{"-id", id, "-title", "T " + id}, strings.NewReader("do it")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(id, agentjob.State{Status: status}); err != nil {
		t.Fatal(err)
	}
}

func TestCmdListFilterAndSummary(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedJobs(t, s, "a", agentjob.Ready)
	seedJobs(t, s, "b", agentjob.Failed)
	seedJobs(t, s, "c", agentjob.Done)

	var all bytes.Buffer
	if err := cmdList(s, nil, &all); err != nil {
		t.Fatal(err)
	}
	got := all.String()
	for _, id := range []string{"a", "b", "c"} {
		if !strings.Contains(got, id) {
			t.Errorf("unfiltered list missing %s:\n%s", id, got)
		}
	}
	if !strings.Contains(got, "# 3 jobs: 1 ready, 1 done, 1 failed") {
		t.Errorf("unfiltered summary wrong:\n%s", got)
	}

	var failed bytes.Buffer
	if err := cmdList(s, []string{"failed"}, &failed); err != nil {
		t.Fatal(err)
	}
	got = failed.String()
	if !strings.Contains(got, "failed     b") {
		t.Errorf("failed filter did not show b:\n%s", got)
	}
	if strings.Contains(got, " a ") || strings.Contains(got, " c ") {
		t.Errorf("failed filter leaked other jobs:\n%s", got)
	}
	// The summary still reports the unfiltered totals.
	if !strings.Contains(got, "# 3 jobs:") {
		t.Errorf("filtered summary lost totals:\n%s", got)
	}

	var none bytes.Buffer
	if err := cmdList(s, []string{"paused"}, &none); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), "# 3 jobs:") {
		t.Errorf("empty filter result should keep summary:\n%s", none.String())
	}
}
