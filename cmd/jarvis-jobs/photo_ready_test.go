package main

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func seedPhotoJob(t *testing.T, s *agentjob.Store, id, status string) {
	t.Helper()
	if err := cmdAdd(s, []string{"-id", id, "-title", "T " + id, "-tags", "photo"}, strings.NewReader("do it")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(id, agentjob.State{Status: status}); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoReady(t *testing.T) {
	s, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idsOf := func(jobs []agentjob.Job) []string {
		ids := make([]string, 0, len(jobs))
		for _, j := range jobs {
			ids = append(ids, j.ID)
		}
		return ids
	}
	if jobs, err := s.PhotoReady(); err != nil || len(jobs) != 0 {
		t.Fatalf("empty store: ids=%v err=%v", jobs, err)
	}

	seedPhotoJob(t, s, "p1", agentjob.Ready)
	seedPhotoJob(t, s, "p2", agentjob.Failed) // tagged but not ready
	seedPhotoJob(t, s, "p3", agentjob.Done)   // tagged and finished
	seedJobs(t, s, "p4", agentjob.Ready)      // ready but not tagged

	if jobs, err := s.PhotoReady(); err != nil {
		t.Fatal(err)
	} else if got := strings.Join(idsOf(jobs), ","); got != "p1" {
		t.Fatalf("PhotoReady = %q, want \"p1\"", got)
	}

	// Drained batch: no ready photo jobs left.
	if err := s.SetStatus("p1", agentjob.State{Status: agentjob.Failed}); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.PhotoReady(); err != nil || len(jobs) != 0 {
		t.Fatalf("drained: ids=%v err=%v", jobs, err)
	}
}
