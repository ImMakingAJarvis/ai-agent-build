package agentjob

import (
	"strings"
	"testing"
	"time"
)

// TestIngestNightlyTaggedIsParked covers NoBrakes 2/6: a nightly-tagged job
// lands parked (out of the active queue), everything else lands ready as
// before, and a re-ingest of the same job changes nothing.
func TestIngestNightlyTaggedIsParked(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	plain := Spec{ID: "p1", Title: "plain", CreatedAt: now}
	nightly := Spec{ID: "n1", Title: "nightly", Tags: []string{TagNightly}, CreatedAt: now}
	for _, sp := range []Spec{plain, nightly} {
		if added, err := s.Ingest(sp); err != nil || !added {
			t.Fatalf("ingest %s: added=%v err=%v", sp.ID, added, err)
		}
	}
	p, err := s.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != Ready {
		t.Errorf("plain job status = %s, want %s", p.Status, Ready)
	}
	n, err := s.Get("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != Parked {
		t.Errorf("nightly job status = %s, want %s", n.Status, Parked)
	}
	// Idempotent re-ingest: the parked state survives a redelivered spec.
	if added, err := s.Ingest(nightly); err != nil || added {
		t.Fatalf("re-ingest: added=%v err=%v, want (false, nil)", added, err)
	}
	if n, _ := s.Get("n1"); n.Status != Parked {
		t.Errorf("re-ingest clobbered status to %s", n.Status)
	}
}

// TestNextNeverReturnsParked: parked jobs sit out of the queue entirely —
// Next skips them, and a queue of only parked jobs is an empty queue.
func TestNextNeverReturnsParked(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.Ingest(Spec{ID: "n1", Title: "nightly", Tags: []string{TagNightly}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Next(); err != nil || j != nil {
		t.Fatalf("Next with only a parked job = %v, %v; want (nil, nil)", j, err)
	}
	if _, err := s.Ingest(Spec{ID: "p1", Title: "plain", Priority: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Next()
	if err != nil || j == nil || j.ID != "p1" {
		t.Fatalf("Next = %v, %v; want the plain job", j, err)
	}
}

// TestParkedNightly: only parked jobs that carry the nightly tag are listed,
// in queue order (priority desc within the band).
func TestParkedNightly(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.Ingest(Spec{ID: "n1", Title: "low", Tags: []string{TagNightly}, Priority: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(Spec{ID: "n2", Title: "high", Tags: []string{TagNightly}, Priority: 5, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// A parked job without the nightly tag (manual SetStatus) is not
	// nightly business.
	if _, err := s.Ingest(Spec{ID: "x1", Title: "parked other", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("x1", State{Status: Parked}); err != nil {
		t.Fatal(err)
	}
	// A nightly-tagged job that is already ready (enqueued) is done.
	if _, err := s.Ingest(Spec{ID: "n3", Title: "already ready", Tags: []string{TagNightly}, Priority: 9, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("n3"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ParkedNightly()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range got {
		ids = append(ids, j.ID)
	}
	if len(ids) != 2 || ids[0] != "n2" || ids[1] != "n1" {
		t.Fatalf("ParkedNightly = %v, want [n2 n1] (queue order)", ids)
	}
}

// TestEnqueue: a parked job moves to ready; anything else is refused, and a
// bad id never reaches the store.
func TestEnqueue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.Ingest(Spec{ID: "n1", Title: "nightly", Tags: []string{TagNightly}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(Spec{ID: "p1", Title: "plain", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("p1"); err == nil || !strings.Contains(err.Error(), "not parked") {
		t.Fatalf("enqueue ready job: err=%v, want not-parked refusal", err)
	}
	if err := s.Enqueue("../etc/passwd"); err == nil {
		t.Fatal("enqueue bad id: want error")
	}
	if err := s.Enqueue("n1"); err != nil {
		t.Fatalf("enqueue parked job: %v", err)
	}
	if j, _ := s.Get("n1"); j.Status != Ready {
		t.Fatalf("after enqueue: %s", j.Status)
	}
	// Second call sees it is no longer parked: no double transition.
	if err := s.Enqueue("n1"); err == nil {
		t.Fatal("second enqueue: want not-parked refusal")
	}
}

// TestRefreshNightlyTagParks: a finished job re-triggered with the nightly
// tag goes back parked (it waits for the next gate), and Refresh still
// refuses jobs that are not finished — including parked ones.
func TestRefreshNightlyTagParks(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sp := Spec{ID: "n1", Title: "nightly", CreatedAt: now}
	if _, err := s.Ingest(sp); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("n1", State{Status: Done, FinishedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	// Re-trigger with the tag: back parked, not ready.
	sp.Tags = []string{TagNightly}
	if err := s.Refresh(sp); err != nil {
		t.Fatalf("refresh done job: %v", err)
	}
	if j, _ := s.Get("n1"); j.Status != Parked {
		t.Fatalf("refresh with nightly tag: %s, want %s", j.Status, Parked)
	}
	// A parked job is not finished: Refresh refuses it.
	if err := s.Refresh(sp); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("refresh parked job: err=%v, want not-finished refusal", err)
	}
	// And without the tag, a finished job comes back ready, as before.
	sp2 := Spec{ID: "p1", Title: "plain", CreatedAt: now}
	if _, err := s.Ingest(sp2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("p1", State{Status: Failed}); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(sp2); err != nil {
		t.Fatalf("refresh failed job: %v", err)
	}
	if j, _ := s.Get("p1"); j.Status != Ready {
		t.Fatalf("refresh without tag: %s, want %s", j.Status, Ready)
	}
}

func TestHasTag(t *testing.T) {
	if (Spec{Tags: []string{"nightly", "other"}}).HasTag(TagNightly) != true {
		t.Error("HasTag(nightly) = false on tagged spec")
	}
	if (Spec{Tags: []string{"other"}}).HasTag(TagNightly) != false {
		t.Error("HasTag(nightly) = true on untagged spec")
	}
	if (Spec{}).HasTag(TagNightly) != false {
		t.Error("HasTag(nightly) = true on zero spec")
	}
}
