package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/sched"
)

func TestAuditJobSpec(t *testing.T) {
	// 2026-11-15 03:00 UTC is a Sunday (2026-11-14) in America/Los_Angeles, so
	// the most recent Monday on/before it is 2026-11-09. We assert against the
	// same local conversion the code uses, so the test is zone-independent.
	at := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	anchor := time.Monday
	sp := auditJobSpec(at, anchor)

	wantWeekKey := sched.WeekKey(at.In(time.Local), anchor)
	if sp.ID != "audit-weekly-"+wantWeekKey {
		t.Errorf("ID = %q, want audit-weekly-%s", sp.ID, wantWeekKey)
	}
	if filepath.Base(sp.ID) != sp.ID {
		t.Errorf("ID %q is not a single path component", sp.ID)
	}
	if sp.Class != agentjob.ClassBackground {
		t.Errorf("Class = %q, want %q", sp.Class, agentjob.ClassBackground)
	}
	if sp.Driver != "local" {
		t.Errorf("Driver = %q, want local", sp.Driver)
	}
	if sp.Source != "sched-audit" {
		t.Errorf("Source = %q, want sched-audit", sp.Source)
	}
	// The instructions must name the report path and all three buckets.
	for _, want := range []string{
		"notes/audits/audit-", wantWeekKey,
		"## Still needed", "## Could use changing", "## Should be removed",
	} {
		if !strings.Contains(sp.Instructions, want) {
			t.Errorf("Instructions missing %q", want)
		}
	}
	// The report date is a YYYY-MM-DD of the tick's local date.
	localDate := at.In(time.Local).Format("2006-01-02")
	if !strings.Contains(sp.Instructions, "audit-"+localDate+".md") {
		t.Errorf("Instructions missing report path for local date %s", localDate)
	}
}

func TestHandleTickEnqueues(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	at := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	e := queue.Event{Kind: queue.SchedKind, Subject: string(sched.Weekly), State: sched.State, At: at}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := handleTick(body, time.Monday, store); err != nil {
		t.Fatalf("handleTick: %v", err)
	}
	sp := auditJobSpec(at, time.Monday)
	jb := filepath.Join(store.Dir(), "jobs", sp.ID, "job.json")
	if _, err := os.Stat(jb); err != nil {
		t.Fatalf("job.json not written for %s: %v", sp.ID, err)
	}
	// A second tick in the same week is a no-op (idempotent) and must not error.
	if err := handleTick(body, time.Monday, store); err != nil {
		t.Fatalf("duplicate handleTick: %v", err)
	}
}

func TestHandleTickRejectsWrongKind(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := queue.Event{Kind: queue.JobKind, Subject: "background", State: "done", At: time.Now().UTC()}
	body, _ := json.Marshal(e)
	if err := handleTick(body, time.Monday, store); err == nil {
		t.Fatal("handleTick(job.* event) = nil, want an error")
	}
}

func TestHandleTickMalformedBody(t *testing.T) {
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := handleTick([]byte("not json"), time.Monday, store); err == nil {
		t.Fatal("handleTick(malformed) = nil, want an error")
	}
}
