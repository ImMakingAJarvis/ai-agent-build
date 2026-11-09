//go:build integration

// Store-level round-trip of the listener CLI commands against the
// switchboard (plan task 15, acceptance 2): `listen` (via attachListener,
// the same code path runListen uses) must create a listener the
// switchboard's own Store.List/Queues sees, `listeners` (via Store.List)
// must list it with its queue/action/target/timeout, and `unlisten` (via
// detachListeners) must remove it — including the --corr narrowing, where
// only the filtered listener goes. The broker half of the loop (the
// switchboard peeking the declared queue) is covered by
// internal/switchboard's integration tests.
package main

import (
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/switchboard"
)

func openTestStore(t *testing.T) *switchboard.Store {
	t.Helper()
	st, err := switchboard.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func TestListenUnlistenRoundTrip(t *testing.T) {
	st := openTestStore(t)

	// listen: flow queue, wake action, timeout, no corr filter.
	l, err := attachListener(st, "flow.plan-x", switchboard.ActionWake, "plan-x", "", "", "", 2*time.Hour)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if l.ID == "" || l.Queue != "flow.plan-x" || l.Action != switchboard.ActionWake {
		t.Fatalf("created listener = %+v", l)
	}

	// The switchboard's own views must see it: List and Queues (what the
	// poll loop iterates).
	all, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != l.ID {
		t.Fatalf("store list = %+v, want the one listener", all)
	}
	if all[0].JobID != "plan-x" || all[0].Timeout != 2*time.Hour {
		t.Errorf("listener fields = %+v, want job plan-x timeout 2h", all[0])
	}
	byQueue, err := st.Queues()
	if err != nil {
		t.Fatalf("queues: %v", err)
	}
	if len(byQueue["flow.plan-x"]) != 1 {
		t.Errorf("queues[flow.plan-x] = %+v, want the listener", byQueue)
	}

	// unlisten: the queue goes quiet again.
	n, err := detachListeners(st, "flow.plan-x", "")
	if err != nil {
		t.Fatalf("unlisten: %v", err)
	}
	if n != 1 {
		t.Fatalf("unlisten removed %d, want 1", n)
	}
	all, err = st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("store after unlisten = %+v, want empty", all)
	}

	// unlisten with nothing there is a no-op, not an error (the job may
	// already have cleaned up its listener).
	if n, err := detachListeners(st, "flow.plan-x", ""); err != nil || n != 0 {
		t.Errorf("second unlisten = %d, %v; want 0, nil", n, err)
	}
}

func TestUnlistenCorrNarrows(t *testing.T) {
	st := openTestStore(t)
	// Two listeners on one queue: one filtered on a corr, one claiming any
	// message. unlisten --corr must remove only the filtered one.
	if _, err := attachListener(st, "flow.plan-y", switchboard.ActionWake, "plan-y", "", "", "plan-y-img-1", 0); err != nil {
		t.Fatalf("listen (corr): %v", err)
	}
	if _, err := attachListener(st, "flow.plan-y", switchboard.ActionWake, "plan-y", "", "", "", 0); err != nil {
		t.Fatalf("listen (any): %v", err)
	}

	n, err := detachListeners(st, "flow.plan-y", "plan-y-img-1")
	if err != nil {
		t.Fatalf("unlisten --corr: %v", err)
	}
	if n != 1 {
		t.Fatalf("unlisten --corr removed %d, want 1", n)
	}
	all, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].CorrFilter != "" {
		t.Errorf("remaining listeners = %+v, want the unfiltered one", all)
	}
}

func TestListenValidationMatchesSwitchboard(t *testing.T) {
	st := openTestStore(t)
	// The CLI must not store what the switchboard would reject: a wake
	// listener without a job, or a forward listener without a target.
	if _, err := attachListener(st, "flow.plan-z", switchboard.ActionWake, "", "", "", "", 0); err == nil {
		t.Error("wake without a job id: expected an error")
	}
	if _, err := attachListener(st, "flow.plan-z", switchboard.ActionForward, "", "", "", "", 0); err == nil {
		t.Error("forward without a target queue: expected an error")
	}
	// A valid forward listener does store, with its target visible to the
	// switchboard's list.
	l, err := attachListener(st, "flow.plan-z", switchboard.ActionForward, "", "fix", "", "", 0)
	if err != nil {
		t.Fatalf("listen forward: %v", err)
	}
	all, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != l.ID || all[0].TargetQueue != "fix" {
		t.Errorf("store = %+v, want the forward listener on fix", all)
	}
}
